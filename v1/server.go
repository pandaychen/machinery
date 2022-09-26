package machinery

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/RichardKnop/machinery/v1/backends/result"
	"github.com/RichardKnop/machinery/v1/brokers/eager"
	"github.com/RichardKnop/machinery/v1/config"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/tasks"
	"github.com/RichardKnop/machinery/v1/tracing"
	"github.com/RichardKnop/machinery/v1/utils"

	backendsiface "github.com/RichardKnop/machinery/v1/backends/iface"
	brokersiface "github.com/RichardKnop/machinery/v1/brokers/iface"
	lockiface "github.com/RichardKnop/machinery/v1/locks/iface"
	opentracing "github.com/opentracing/opentracing-go"
)

// Server is the main Machinery object and stores all configuration
// All the tasks workers process are registered against the server

//Machinery的业务主体
type Server struct {
	config            *config.Config //关联配置
	registeredTasks   *sync.Map
	broker            brokersiface.Broker   //关联通用broker
	backend           backendsiface.Backend //关联通用backend
	lock              lockiface.Lock        //关联通用的lock（本地锁/redis锁）
	scheduler         *cron.Cron            //包含一个内存的cron
	prePublishHandler func(*tasks.Signature)
}

// NewServerWithBrokerBackend ...
func NewServerWithBrokerBackendLock(cnf *config.Config, brokerServer brokersiface.Broker, backendServer backendsiface.Backend, lock lockiface.Lock) *Server {
	srv := &Server{
		config:          cnf,
		registeredTasks: new(sync.Map),
		broker:          brokerServer,
		backend:         backendServer,
		lock:            lock,
		scheduler:       cron.New(),
	}

	// Run scheduler job
	//https://github.com/robfig/cron/blob/master/cron.go#L226
	go srv.scheduler.Run()

	return srv
}

// NewServer creates Server instance
func NewServer(cnf *config.Config) (*Server, error) {
	broker, err := BrokerFactory(cnf)
	if err != nil {
		return nil, err
	}

	// Backend is optional so we ignore the error
	backend, _ := BackendFactory(cnf)

	// Init lock
	lock, err := LockFactory(cnf)
	if err != nil {
		return nil, err
	}

	srv := NewServerWithBrokerBackendLock(cnf, broker, backend, lock)

	// init for eager-mode
	eager, ok := broker.(eager.Mode)
	if ok {
		// we don't have to call worker.Launch in eager mode
		eager.AssignWorker(srv.NewWorker("eager", 0))
	}

	return srv, nil
}

// NewWorker creates Worker instance
//	Sever 建立 Worker
func (server *Server) NewWorker(consumerTag string, concurrency int) *Worker {
	return &Worker{
		server:      server,
		ConsumerTag: consumerTag,
		Concurrency: concurrency,
		Queue:       "",
	}
}

// NewCustomQueueWorker creates Worker instance with Custom Queue
// Sever 建立 Worker的第二种方法：NewCustomQueueWorker，与NewWorker的最大区别，就是queue可以指定，避免所有的任务都注册到系统默认的队列中去
func (server *Server) NewCustomQueueWorker(consumerTag string, concurrency int, queue string) *Worker {
	return &Worker{
		server:      server,
		ConsumerTag: consumerTag,
		Concurrency: concurrency,
		Queue:       queue,
	}
}

// GetBroker returns broker
func (server *Server) GetBroker() brokersiface.Broker {
	return server.broker
}

// SetBroker sets broker
func (server *Server) SetBroker(broker brokersiface.Broker) {
	server.broker = broker
}

// GetBackend returns backend
func (server *Server) GetBackend() backendsiface.Backend {
	return server.backend
}

// SetBackend sets backend
func (server *Server) SetBackend(backend backendsiface.Backend) {
	server.backend = backend
}

// GetConfig returns connection object
func (server *Server) GetConfig() *config.Config {
	return server.config
}

// SetConfig sets config
func (server *Server) SetConfig(cnf *config.Config) {
	server.config = cnf
}

// SetPreTaskHandler Sets pre publish handler
func (server *Server) SetPreTaskHandler(handler func(*tasks.Signature)) {
	server.prePublishHandler = handler
}

// RegisterTasks registers all tasks at once
// 注册任务：这个方法比较有趣，把任务编排所需要的所有操作方法都注册到Sever结构中的registeredTasks，在需要使用的时候Sever会在里面查找
func (server *Server) RegisterTasks(namedTaskFuncs map[string]interface{}) error {
	for _, task := range namedTaskFuncs {
		if err := tasks.ValidateTask(task); err != nil {
			return err
		}
	}

	for k, v := range namedTaskFuncs {
		server.registeredTasks.Store(k, v)
	}

	server.broker.SetRegisteredTaskNames(server.GetRegisteredTaskNames())
	return nil
}

// RegisterTask registers a single task
func (server *Server) RegisterTask(name string, taskFunc interface{}) error {
	if err := tasks.ValidateTask(taskFunc); err != nil {
		return err
	}
	server.registeredTasks.Store(name, taskFunc)
	server.broker.SetRegisteredTaskNames(server.GetRegisteredTaskNames())
	return nil
}

// IsTaskRegistered returns true if the task name is registered with this broker
func (server *Server) IsTaskRegistered(name string) bool {
	_, ok := server.registeredTasks.Load(name)
	return ok
}

// GetRegisteredTask returns registered task by name
// 根据名字获取已经注册的执行函数
func (server *Server) GetRegisteredTask(name string) (interface{}, error) {
	taskFunc, ok := server.registeredTasks.Load(name)
	if !ok {
		return nil, fmt.Errorf("Task not registered error: %s", name)
	}
	return taskFunc, nil
}

// SendTaskWithContext will inject the trace context in the signature headers before publishing it
func (server *Server) SendTaskWithContext(ctx context.Context, signature *tasks.Signature) (*result.AsyncResult, error) {
	span, _ := opentracing.StartSpanFromContext(ctx, "SendTask", tracing.ProducerOption(), tracing.MachineryTag)
	defer span.Finish()

	// tag the span with some info about the signature
	signature.Headers = tracing.HeadersWithSpan(signature.Headers, span)

	// Make sure result backend is defined
	if server.backend == nil {
		return nil, errors.New("Result backend required")
	}

	// Auto generate a UUID if not set already
	if signature.UUID == "" {
		taskID := uuid.New().String()
		signature.UUID = fmt.Sprintf("task_%v", taskID)
	}

	// Set initial task state to PENDING
	if err := server.backend.SetStatePending(signature); err != nil {
		return nil, fmt.Errorf("Set state pending error: %s", err)
	}

	if server.prePublishHandler != nil {
		server.prePublishHandler(signature)
	}

	if err := server.broker.Publish(ctx, signature); err != nil {
		return nil, fmt.Errorf("Publish message error: %s", err)
	}

	return result.NewAsyncResult(signature, server.backend), nil
}

// SendTask publishes a task to the default queue
func (server *Server) SendTask(signature *tasks.Signature) (*result.AsyncResult, error) {
	return server.SendTaskWithContext(context.Background(), signature)
}

// SendChainWithContext will inject the trace context in all the signature headers before publishing it
func (server *Server) SendChainWithContext(ctx context.Context, chain *tasks.Chain) (*result.ChainAsyncResult, error) {
	span, _ := opentracing.StartSpanFromContext(ctx, "SendChain", tracing.ProducerOption(), tracing.MachineryTag, tracing.WorkflowChainTag)
	defer span.Finish()

	tracing.AnnotateSpanWithChainInfo(span, chain)

	return server.SendChain(chain)
}

// SendChain triggers a chain of tasks
func (server *Server) SendChain(chain *tasks.Chain) (*result.ChainAsyncResult, error) {
	_, err := server.SendTask(chain.Tasks[0])
	if err != nil {
		return nil, err
	}

	return result.NewChainAsyncResult(chain.Tasks, server.backend), nil
}

// SendGroupWithContext will inject the trace context in all the signature headers before publishing it

// 向Worker发送group分组任务（sendConcurrency为发送并发压制）
func (server *Server) SendGroupWithContext(ctx context.Context, group *tasks.Group, sendConcurrency int) ([]*result.AsyncResult, error) {
	span, _ := opentracing.StartSpanFromContext(ctx, "SendGroup", tracing.ProducerOption(), tracing.MachineryTag, tracing.WorkflowGroupTag)
	defer span.Finish()

	tracing.AnnotateSpanWithGroupInfo(span, group, sendConcurrency)

	// Make sure result backend is defined
	if server.backend == nil {
		return nil, errors.New("Result backend required")
	}

	asyncResults := make([]*result.AsyncResult, len(group.Tasks))

	var wg sync.WaitGroup
	wg.Add(len(group.Tasks))
	errorsChan := make(chan error, len(group.Tasks)*2)

	// Init group
	server.backend.InitGroup(group.GroupUUID, group.GetUUIDs())

	// Init the tasks Pending state first
	// 初始化组中的任务初始状态
	for _, signature := range group.Tasks {
		if err := server.backend.SetStatePending(signature); err != nil {
			errorsChan <- err
			continue
		}
	}

	pool := make(chan struct{}, sendConcurrency)
	go func() {
		for i := 0; i < sendConcurrency; i++ {
			pool <- struct{}{}
		}
	}()

	for i, signature := range group.Tasks {

		if sendConcurrency > 0 {
			<-pool
		}

		//并发的把group数据publish到server
		go func(s *tasks.Signature, index int) {
			defer wg.Done()

			// Publish task

			err := server.broker.Publish(ctx, s)

			if sendConcurrency > 0 {
				pool <- struct{}{}
			}

			if err != nil {
				errorsChan <- fmt.Errorf("Publish message error: %s", err)
				return
			}

			//结果存储
			asyncResults[index] = result.NewAsyncResult(s, server.backend)
		}(signature, i)
	}

	done := make(chan int)
	go func() {
		wg.Wait()
		done <- 1
	}()

	//等待获取结果
	select {
	case err := <-errorsChan:
		return asyncResults, err
	case <-done:
		return asyncResults, nil
	}
}

// SendGroup triggers a group of parallel tasks
func (server *Server) SendGroup(group *tasks.Group, sendConcurrency int) ([]*result.AsyncResult, error) {
	return server.SendGroupWithContext(context.Background(), group, sendConcurrency)
}

// SendChordWithContext will inject the trace context in all the signature headers before publishing it
func (server *Server) SendChordWithContext(ctx context.Context, chord *tasks.Chord, sendConcurrency int) (*result.ChordAsyncResult, error) {
	span, _ := opentracing.StartSpanFromContext(ctx, "SendChord", tracing.ProducerOption(), tracing.MachineryTag, tracing.WorkflowChordTag)
	defer span.Finish()

	tracing.AnnotateSpanWithChordInfo(span, chord, sendConcurrency)

	_, err := server.SendGroupWithContext(ctx, chord.Group, sendConcurrency)
	if err != nil {
		return nil, err
	}

	return result.NewChordAsyncResult(
		chord.Group.Tasks,
		chord.Callback,
		server.backend,
	), nil
}

// SendChord triggers a group of parallel tasks with a callback
func (server *Server) SendChord(chord *tasks.Chord, sendConcurrency int) (*result.ChordAsyncResult, error) {
	return server.SendChordWithContext(context.Background(), chord, sendConcurrency)
}

// GetRegisteredTaskNames returns slice of registered task names
func (server *Server) GetRegisteredTaskNames() []string {
	taskNames := make([]string, 0)

	server.registeredTasks.Range(func(key, value interface{}) bool {
		taskNames = append(taskNames, key.(string))
		return true
	})
	return taskNames
}

// RegisterPeriodicTask register a periodic task which will be triggered periodically

// 封装的cron的注册方法：向cron注册定时调度事件
//注意，仅仅注册在内存，重启会丢失？
func (server *Server) RegisterPeriodicTask(spec, name string, signature *tasks.Signature) error {
	//check spec
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return err
	}

	f := func() {
		//get lock
		// 锁的实现，可以参考下
		// LockWithRetries的参数1：排他ID
		//LockWithRetries的参数2：是什么意思？
		err := server.lock.LockWithRetries(utils.GetLockName(name, spec), schedule.Next(time.Now()).UnixNano()-1)
		if err != nil {
			return
		}

		//send task
		_, err = server.SendTask(tasks.CopySignature(signature))
		if err != nil {
			log.ERROR.Printf("periodic task failed. task name is: %s. error is %s", name, err.Error())
		}
	}

	//注册cron，以及到期触发的事件处理方法f
	_, err = server.scheduler.AddFunc(spec, f)
	return err
}

// RegisterPeriodicChain register a periodic chain which will be triggered periodically
func (server *Server) RegisterPeriodicChain(spec, name string, signatures ...*tasks.Signature) error {
	//check spec
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return err
	}

	f := func() {
		// new chain
		chain, _ := tasks.NewChain(tasks.CopySignatures(signatures...)...)

		//get lock
		err := server.lock.LockWithRetries(utils.GetLockName(name, spec), schedule.Next(time.Now()).UnixNano()-1)
		if err != nil {
			return
		}

		//send task
		_, err = server.SendChain(chain)
		if err != nil {
			log.ERROR.Printf("periodic task failed. task name is: %s. error is %s", name, err.Error())
		}
	}

	_, err = server.scheduler.AddFunc(spec, f)
	return err
}

// RegisterPeriodicGroup register a periodic group which will be triggered periodically
func (server *Server) RegisterPeriodicGroup(spec, name string, sendConcurrency int, signatures ...*tasks.Signature) error {
	//check spec
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return err
	}

	f := func() {
		// new group
		group, _ := tasks.NewGroup(tasks.CopySignatures(signatures...)...)

		//get lock
		err := server.lock.LockWithRetries(utils.GetLockName(name, spec), schedule.Next(time.Now()).UnixNano()-1)
		if err != nil {
			return
		}

		//send task
		_, err = server.SendGroup(group, sendConcurrency)
		if err != nil {
			log.ERROR.Printf("periodic task failed. task name is: %s. error is %s", name, err.Error())
		}
	}

	_, err = server.scheduler.AddFunc(spec, f)
	return err
}

// RegisterPeriodicChord register a periodic chord which will be triggered periodically
func (server *Server) RegisterPeriodicChord(spec, name string, sendConcurrency int, callback *tasks.Signature, signatures ...*tasks.Signature) error {
	//check spec
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return err
	}

	f := func() {
		// new chord
		group, _ := tasks.NewGroup(tasks.CopySignatures(signatures...)...)
		chord, _ := tasks.NewChord(group, tasks.CopySignature(callback))

		//get lock
		err := server.lock.LockWithRetries(utils.GetLockName(name, spec), schedule.Next(time.Now()).UnixNano()-1)
		if err != nil {
			return
		}

		//send task
		_, err = server.SendChord(chord, sendConcurrency)
		if err != nil {
			log.ERROR.Printf("periodic task failed. task name is: %s. error is %s", name, err.Error())
		}
	}

	_, err = server.scheduler.AddFunc(spec, f)
	return err
}
