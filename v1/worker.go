package machinery

//machinery的worker实现

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/opentracing/opentracing-go"

	"github.com/RichardKnop/machinery/v1/backends/amqp"
	"github.com/RichardKnop/machinery/v1/brokers/errs"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/retry"
	"github.com/RichardKnop/machinery/v1/tasks"
	"github.com/RichardKnop/machinery/v1/tracing"
)

// Worker represents a single worker process
type Worker struct {
	server            *Server //worker可以调用Server的各种实现方法（server中记录了各种执行任务的方法和数据获取接口-broker，结构处理接口backend等等）
	ConsumerTag       string
	Concurrency       int                    //启动一个 worker 执行任务的并发量最大值，默认为当前cpu核数2倍
	Queue             string                 //任务触发队列存储 Key，用于worker处理不同队列的消息（类似topic）
	errorHandler      func(err error)        //errorHandler比较重要
	preTaskHandler    func(*tasks.Signature) //执行任务前做的处理函数，详见worker.Process
	postTaskHandler   func(*tasks.Signature) //执行任务后做的处理函数，详见worker.Process
	preConsumeHandler func(*Worker) bool     //消费任务前的判断任务是否可消费逻辑，详见broker.ConsumeOne
}

var (
	// ErrWorkerQuitGracefully is return when worker quit gracefully
	ErrWorkerQuitGracefully = errors.New("Worker quit gracefully")
	// ErrWorkerQuitGracefully is return when worker quit abruptly
	ErrWorkerQuitAbruptly = errors.New("Worker quit abruptly")
)

// Launch starts a new worker process. The worker subscribes
// to the default queue and processes incoming registered tasks

// 启动worker，返回一个异步的阻塞error channel
func (worker *Worker) Launch() error {
	errorsChan := make(chan error)

	//启动一个异步执行的worker，Loop执行任务
	worker.LaunchAsync(errorsChan)

	return <-errorsChan
}

// LaunchAsync is a non blocking version of Launch
func (worker *Worker) LaunchAsync(errorsChan chan<- error) {

	//调用server的方法，获取配置及broker等基础信息
	cnf := worker.server.GetConfig()
	broker := worker.server.GetBroker()

	// Log some useful information about worker configuration
	log.INFO.Printf("Launching a worker with the following settings:")
	log.INFO.Printf("- Broker: %s", RedactURL(cnf.Broker))
	if worker.Queue == "" {
		log.INFO.Printf("- DefaultQueue: %s", cnf.DefaultQueue)
	} else {
		log.INFO.Printf("- CustomQueue: %s", worker.Queue)
	}
	log.INFO.Printf("- ResultBackend: %s", RedactURL(cnf.ResultBackend))
	if cnf.AMQP != nil {
		log.INFO.Printf("- AMQP: %s", cnf.AMQP.Exchange)
		log.INFO.Printf("  - Exchange: %s", cnf.AMQP.Exchange)
		log.INFO.Printf("  - ExchangeType: %s", cnf.AMQP.ExchangeType)
		log.INFO.Printf("  - BindingKey: %s", cnf.AMQP.BindingKey)
		log.INFO.Printf("  - PrefetchCount: %d", cnf.AMQP.PrefetchCount)
	}

	var signalWG sync.WaitGroup
	// Goroutine to start broker consumption and handle retries when broker connection dies

	//worker中会开一个独立goroutine维护一个broker消费者，用errchan阻塞住，维护消费者的同时监听一个系统的信号管道做优雅退出，
	go func() {
		for {
			//StartConsuming：任务处理

			//这里特别注意第三个参数：worker，以iface.TaskProcessor类型的参数传入，最终在执行任务的时候被调用（worker.Process方法）
			retry, err := broker.StartConsuming(worker.ConsumerTag, worker.Concurrency, worker)

			if retry {
				if worker.errorHandler != nil {
					//如果用户定义了errorHandler
					worker.errorHandler(err)
				} else {
					log.WARNING.Printf("Broker failed with error: %s", err)
				}
			} else {
				signalWG.Wait()
				errorsChan <- err // stop the goroutine
				return
			}
		}
	}()
	if !cnf.NoUnixSignals {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		var signalsReceived uint

		// Goroutine Handle SIGINT and SIGTERM signals
		go func() {
			for s := range sig {
				log.WARNING.Printf("Signal received: %v", s)
				signalsReceived++

				if signalsReceived < 2 {
					// After first Ctrl+C start quitting the worker gracefully
					log.WARNING.Print("Waiting for running tasks to finish before shutting down")
					signalWG.Add(1)
					go func() {
						worker.Quit()
						errorsChan <- ErrWorkerQuitGracefully
						signalWG.Done()
					}()
				} else {
					// Abort the program when user hits Ctrl+C second time in a row
					errorsChan <- ErrWorkerQuitAbruptly
				}
			}
		}()
	}
}

// CustomQueue returns Custom Queue of the running worker process
func (worker *Worker) CustomQueue() string {
	return worker.Queue
}

// Quit tears down the running worker process
func (worker *Worker) Quit() {
	worker.server.GetBroker().StopConsuming()
}

// Process handles received tasks and triggers success/error callbacks

// 任务会被PROCESS处理，参数为signature *tasks.Signature
func (worker *Worker) Process(signature *tasks.Signature) error {
	// If the task is not registered with this worker, do not continue
	// but only return nil as we do not want to restart the worker process
	if !worker.server.IsTaskRegistered(signature.Name) {
		return nil
	}

	//查询对应signature.Name的注册处理方法
	taskFunc, err := worker.server.GetRegisteredTask(signature.Name)
	if err != nil {
		return nil
	}

	// Update task state to RECEIVED
	if err = worker.server.GetBackend().SetStateReceived(signature); err != nil {
		return fmt.Errorf("Set state to 'received' for task %s returned error: %s", signature.UUID, err)
	}

	// Prepare task for processing

	//！！根据taskFunc，以及原始任务signature，构造一个新的task结构
	task, err := tasks.NewWithSignature(taskFunc, signature)
	// if this failed, it means the task is malformed, probably has invalid
	// signature, go directly to task failed without checking whether to retry
	if err != nil {
		worker.taskFailed(signature, err)
		return err
	}

	// try to extract trace span from headers and add it to the function context
	// so it can be used inside the function if it has context.Context as the first
	// argument. Start a new span if it isn't found.
	taskSpan := tracing.StartSpanFromHeaders(signature.Headers, signature.Name)
	tracing.AnnotateSpanWithSignatureInfo(taskSpan, signature)
	task.Context = opentracing.ContextWithSpan(task.Context, taskSpan)

	// Update task state to STARTED
	if err = worker.server.GetBackend().SetStateStarted(signature); err != nil {
		return fmt.Errorf("Set state to 'started' for task %s returned error: %s", signature.UUID, err)
	}

	//Run handler before the task is called
	if worker.preTaskHandler != nil {
		//处理任务前，hook
		worker.preTaskHandler(signature)
	}

	//Defer run handler for the end of the task
	if worker.postTaskHandler != nil {
		//处理任务后，hook，通过defer
		defer worker.postTaskHandler(signature)
	}

	// Call the task
	//处理任务
	results, err := task.Call()

	//处理完成，根据处理任务的结果和报错调用taskfailed和taskSucceeded，使用backend接口进行记录
	if err != nil {
		// If a tasks.ErrRetryTaskLater was returned from the task,
		// retry the task after specified duration

		//对任务重试的处理机制
		retriableErr, ok := interface{}(err).(tasks.ErrRetryTaskLater)
		if ok {
			return worker.retryTaskIn(signature, retriableErr.RetryIn())
		}

		// Otherwise, execute default retry logic based on signature.RetryCount
		// and signature.RetryTimeout values
		if signature.RetryCount > 0 {
			return worker.taskRetry(signature)
		}

		return worker.taskFailed(signature, err)
	}

	//记录处理成功
	return worker.taskSucceeded(signature, results)
}

// retryTask decrements RetryCount counter and republishes the task to the queue
func (worker *Worker) taskRetry(signature *tasks.Signature) error {
	// Update task state to RETRY
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("Set state to 'retry' for task %s returned error: %s", signature.UUID, err)
	}

	// Decrement the retry counter, when it reaches 0, we won't retry again
	signature.RetryCount--

	// Increase retry timeout
	signature.RetryTimeout = retry.FibonacciNext(signature.RetryTimeout)

	// Delay task by signature.RetryTimeout seconds
	eta := time.Now().UTC().Add(time.Second * time.Duration(signature.RetryTimeout))
	signature.ETA = &eta

	log.WARNING.Printf("Task %s failed. Going to retry in %d seconds.", signature.UUID, signature.RetryTimeout)

	// Send the task back to the queue
	_, err := worker.server.SendTask(signature)
	return err
}

// taskRetryIn republishes the task to the queue with ETA of now + retryIn.Seconds()
func (worker *Worker) retryTaskIn(signature *tasks.Signature, retryIn time.Duration) error {
	// Update task state to RETRY
	if err := worker.server.GetBackend().SetStateRetry(signature); err != nil {
		return fmt.Errorf("Set state to 'retry' for task %s returned error: %s", signature.UUID, err)
	}

	// Delay task by retryIn duration
	eta := time.Now().UTC().Add(retryIn)
	signature.ETA = &eta

	log.WARNING.Printf("Task %s failed. Going to retry in %.0f seconds.", signature.UUID, retryIn.Seconds())

	// Send the task back to the queue
	_, err := worker.server.SendTask(signature)
	return err
}

// taskSucceeded updates the task state and triggers success callbacks or a
// chord callback if this was the last task of a group with a chord callback
func (worker *Worker) taskSucceeded(signature *tasks.Signature, taskResults []*tasks.TaskResult) error {
	// Update task state to SUCCESS
	if err := worker.server.GetBackend().SetStateSuccess(signature, taskResults); err != nil {
		return fmt.Errorf("Set state to 'success' for task %s returned error: %s", signature.UUID, err)
	}

	// Log human readable results of the processed task
	var debugResults = "[]"
	results, err := tasks.ReflectTaskResults(taskResults)
	if err != nil {
		log.WARNING.Print(err)
	} else {
		debugResults = tasks.HumanReadableResults(results)
	}
	log.DEBUG.Printf("Processed task %s. Results = %s", signature.UUID, debugResults)

	// Trigger success callbacks

	for _, successTask := range signature.OnSuccess {
		if signature.Immutable == false {
			// Pass results of the task to success callbacks
			for _, taskResult := range taskResults {
				successTask.Args = append(successTask.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}

		worker.server.SendTask(successTask)
	}

	// If the task was not part of a group, just return
	if signature.GroupUUID == "" {
		return nil
	}

	// There is no chord callback, just return
	if signature.ChordCallback == nil {
		return nil
	}

	// Check if all task in the group has completed
	groupCompleted, err := worker.server.GetBackend().GroupCompleted(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		return fmt.Errorf("Completed check for group %s returned error: %s", signature.GroupUUID, err)
	}

	// If the group has not yet completed, just return
	if !groupCompleted {
		return nil
	}

	// Defer purging of group meta queue if we are using AMQP backend
	if worker.hasAMQPBackend() {
		defer worker.server.GetBackend().PurgeGroupMeta(signature.GroupUUID)
	}

	// Trigger chord callback
	shouldTrigger, err := worker.server.GetBackend().TriggerChord(signature.GroupUUID)
	if err != nil {
		return fmt.Errorf("Triggering chord for group %s returned error: %s", signature.GroupUUID, err)
	}

	// Chord has already been triggered
	if !shouldTrigger {
		return nil
	}

	// Get task states
	taskStates, err := worker.server.GetBackend().GroupTaskStates(
		signature.GroupUUID,
		signature.GroupTaskCount,
	)
	if err != nil {
		log.ERROR.Printf(
			"Failed to get tasks states for group:[%s]. Task count:[%d]. The chord may not be triggered. Error:[%s]",
			signature.GroupUUID,
			signature.GroupTaskCount,
			err,
		)
		return nil
	}

	// Append group tasks' return values to chord task if it's not immutable
	for _, taskState := range taskStates {
		if !taskState.IsSuccess() {
			return nil
		}

		if signature.ChordCallback.Immutable == false {
			// Pass results of the task to the chord callback
			for _, taskResult := range taskState.Results {
				signature.ChordCallback.Args = append(signature.ChordCallback.Args, tasks.Arg{
					Type:  taskResult.Type,
					Value: taskResult.Value,
				})
			}
		}
	}

	// Send the chord task
	_, err = worker.server.SendTask(signature.ChordCallback)
	if err != nil {
		return err
	}

	return nil
}

// taskFailed updates the task state and triggers error callbacks
func (worker *Worker) taskFailed(signature *tasks.Signature, taskErr error) error {
	// Update task state to FAILURE
	if err := worker.server.GetBackend().SetStateFailure(signature, taskErr.Error()); err != nil {
		return fmt.Errorf("Set state to 'failure' for task %s returned error: %s", signature.UUID, err)
	}

	if worker.errorHandler != nil {
		worker.errorHandler(taskErr)
	} else {
		log.ERROR.Printf("Failed processing task %s. Error = %v", signature.UUID, taskErr)
	}

	// Trigger error callbacks
	for _, errorTask := range signature.OnError {
		// Pass error as a first argument to error callbacks
		args := append([]tasks.Arg{{
			Type:  "string",
			Value: taskErr.Error(),
		}}, errorTask.Args...)
		errorTask.Args = args
		worker.server.SendTask(errorTask)
	}

	if signature.StopTaskDeletionOnError {
		return errs.ErrStopTaskDeletion
	}

	return nil
}

// Returns true if the worker uses AMQP backend
func (worker *Worker) hasAMQPBackend() bool {
	_, ok := worker.server.GetBackend().(*amqp.Backend)
	return ok
}

// SetErrorHandler sets a custom error handler for task errors
// A default behavior is just to log the error after all the retry attempts fail
func (worker *Worker) SetErrorHandler(handler func(err error)) {
	worker.errorHandler = handler
}

//SetPreTaskHandler sets a custom handler func before a job is started
func (worker *Worker) SetPreTaskHandler(handler func(*tasks.Signature)) {
	worker.preTaskHandler = handler
}

//SetPostTaskHandler sets a custom handler for the end of a job
func (worker *Worker) SetPostTaskHandler(handler func(*tasks.Signature)) {
	worker.postTaskHandler = handler
}

//SetPreConsumeHandler sets a custom handler for the end of a job
func (worker *Worker) SetPreConsumeHandler(handler func(*Worker) bool) {
	worker.preConsumeHandler = handler
}

//GetServer returns server
func (worker *Worker) GetServer() *Server {
	return worker.server
}

//
func (worker *Worker) PreConsumeHandler() bool {
	if worker.preConsumeHandler == nil {
		return true
	}

	return worker.preConsumeHandler(worker)
}

func RedactURL(urlString string) string {
	u, err := url.Parse(urlString)
	if err != nil {
		return urlString
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}
