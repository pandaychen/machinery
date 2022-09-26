##  v1

基于v1版本的源码注释



##  任务的特点
-   任务重试机制
-   延迟任务机制
-   任务定时机制
-   任务回调机制
-   任务结果记录


##  Server
-   `RegisterTasks`的方法就是将自己任务编排所需要的操作方法都注册到Sever结构中的registeredTasks，在需要使用的时候Sever会在里面查找；
-   `Send*`方法其实是发送任务各种不同编排姿势的任务到Worker中执行

```GOLANG
type Server interface {
	GetBroker() brokersiface.Broker
	GetConfig() *config.Config

	RegisterTasks(namedTaskFuncs map[string]interface{}) error


	SendTaskWithContext(ctx context.Context, signature *tasks.Signature) (*result.AsyncResult, error)
	SendTask(signature *tasks.Signature) (*result.AsyncResult, error)
	SendChainWithContext(ctx context.Context, chain *tasks.Chain) (*result.ChainAsyncResult, error)
	SendChain(chain *tasks.Chain) (*result.ChainAsyncResult, error)
	SendGroupWithContext(ctx context.Context, group *tasks.Group, sendConcurrency int) ([]*result.AsyncResult, error)
	SendGroup(group *tasks.Group, sendConcurrency int) ([]*result.AsyncResult, error)
	SendChordWithContext(ctx context.Context, chord *tasks.Chord, sendConcurrency int) (*result.ChordAsyncResult, error)
	SendChord(chord *tasks.Chord, sendConcurrency int) (*result.ChordAsyncResult, error)
}
```

##	Worker
-	Worker 是整个machinery架构的执行主体，负责根据不同的任务及编排模式去获取任务并执行，最终输出结果并记录
-	Worker可以单独启动


Worker的实现：
```golang
// Launch starts a new worker process. The worker subscribes
// to the default queue and processes incoming registered tasks
func (worker *Worker) Launch() error {}

// LaunchAsync is a non blocking version of Launch
func (worker *Worker) LaunchAsync(errorsChan chan<- error) {}

// CustomQueue returns Custom Queue of the running worker process
func (worker *Worker) CustomQueue() string {}

// Quit tears down the running worker process
func (worker *Worker) Quit() {}

// Process handles received tasks and triggers success/error callbacks
func (worker *Worker) Process(signature *tasks.Signature) error {}

// retryTask decrements RetryCount counter and republishes the task to the queue
func (worker *Worker) taskRetry(signature *tasks.Signature) error {}

// taskRetryIn republishes the task to the queue with ETA of now + retryIn.Seconds()
func (worker *Worker) retryTaskIn(signature *tasks.Signature, retryIn time.Duration) error {}

// taskSucceeded updates the task state and triggers success callbacks or a
// chord callback if this was the last task of a group with a chord callback
func (worker *Worker) taskSucceeded(signature *tasks.Signature, taskResults []*tasks.TaskResult) error {}

// taskFailed updates the task state and triggers error callbacks
func (worker *Worker) taskFailed(signature *tasks.Signature, taskErr error) error {}

// Returns true if the worker uses AMQP backend
func (worker *Worker) hasAMQPBackend() bool {}

// SetErrorHandler sets a custom error handler for task errors
// A default behavior is just to log the error after all the retry attempts fail
func (worker *Worker) SetErrorHandler(handler func(err error)) {}

//SetPreTaskHandler sets a custom handler func before a job is started
func (worker *Worker) SetPreTaskHandler(handler func(*tasks.Signature)) {}

//SetPostTaskHandler sets a custom handler for the end of a job
func (worker *Worker) SetPostTaskHandler(handler func(*tasks.Signature)) {}

//SetPreConsumeHandler sets a custom handler for the end of a job
func (worker *Worker) SetPreConsumeHandler(handler func(*Worker) bool) {}

//GetServer returns server
func (worker *Worker) GetServer() *Server {}
func (worker *Worker) PreConsumeHandler() bool {}
func RedactURL(urlString string) string {}
```

##	Task：封装的任务执行
Task：任务（signature）+执行方法+上下文+任务执行参数的封装


这里比较晦涩的是对参数relect的处理，需要保证对Args 的通用化：

1、支持的参数注册<br>
```GO
typesMap = map[string]reflect.Type{
	// base types
	"bool":    reflect.TypeOf(true),
	"int":     reflect.TypeOf(int(1)),
	"int8":    reflect.TypeOf(int8(1)),
	"int16":   reflect.TypeOf(int16(1)),
	"int32":   reflect.TypeOf(int32(1)),
	"int64":   reflect.TypeOf(int64(1)),
	"uint":    reflect.TypeOf(uint(1)),
	"uint8":   reflect.TypeOf(uint8(1)),
	"uint16":  reflect.TypeOf(uint16(1)),
	"uint32":  reflect.TypeOf(uint32(1)),
	"uint64":  reflect.TypeOf(uint64(1)),
	"float32": reflect.TypeOf(float32(0.5)),
	"float64": reflect.TypeOf(float64(0.5)),
	"string":  reflect.TypeOf(string("")),
	// slices
	"[]bool":    reflect.TypeOf(make([]bool, 0)),
	"[]int":     reflect.TypeOf(make([]int, 0)),
	"[]int8":    reflect.TypeOf(make([]int8, 0)),
	"[]int16":   reflect.TypeOf(make([]int16, 0)),
	"[]int32":   reflect.TypeOf(make([]int32, 0)),
	"[]int64":   reflect.TypeOf(make([]int64, 0)),
	"[]uint":    reflect.TypeOf(make([]uint, 0)),
	"[]uint8":   reflect.TypeOf(make([]uint8, 0)),
	"[]uint16":  reflect.TypeOf(make([]uint16, 0)),
	"[]uint32":  reflect.TypeOf(make([]uint32, 0)),
	"[]uint64":  reflect.TypeOf(make([]uint64, 0)),
	"[]float32": reflect.TypeOf(make([]float32, 0)),
	"[]float64": reflect.TypeOf(make([]float64, 0)),
	"[]byte":    reflect.TypeOf(make([]byte, 0)),
	"[]string":  reflect.TypeOf([]string{""}),
}
```

2、参数通用化<br>
从`ReflectArgs`可知，这里对参数的处理是deepCopy
```GOLANG
// ReflectArgs converts []TaskArg to []reflect.Value
func (t *Task) ReflectArgs(args []Arg) error {
	argValues := make([]reflect.Value, len(args))

	for i, arg := range args {
		argValue, err := ReflectValue(arg.Type, arg.Value)
		if err != nil {
			return err
		}
		argValues[i] = argValue
	}

	t.Args = argValues
	return nil
}
```

3、处理每个参数`arg.Type`, `arg.Value`<br>
```GO
// ReflectValue converts interface{} to reflect.Value based on string type
func ReflectValue(valueType string, value interface{}) (reflect.Value, error) {
	if strings.HasPrefix(valueType, "[]") {
		//处理slice
		return reflectValues(valueType, value)
	}

	return reflectValue(valueType, value)
}
```

4、普通类型处理<br>
```GOLANG
// reflectValue converts interface{} to reflect.Value based on string type
// representing a base type (not a slice)
func reflectValue(valueType string, value interface{}) (reflect.Value, error) {
	theType, ok := typesMap[valueType]
	if !ok {
		return reflect.Value{}, NewErrUnsupportedType(valueType)
	}
	theValue := reflect.New(theType)

	// Booleans
	if theType.String() == "bool" {
		boolValue, err := getBoolValue(theType.String(), value)
		if err != nil {
			return reflect.Value{}, err
		}

		theValue.Elem().SetBool(boolValue)
		return theValue.Elem(), nil
	}

	// Integers
	if strings.HasPrefix(theType.String(), "int") {
		intValue, err := getIntValue(theType.String(), value)
		if err != nil {
			return reflect.Value{}, err
		}

		theValue.Elem().SetInt(intValue)
		return theValue.Elem(), err
	}

	// Unsigned integers
	if strings.HasPrefix(theType.String(), "uint") {
		uintValue, err := getUintValue(theType.String(), value)
		if err != nil {
			return reflect.Value{}, err
		}

		theValue.Elem().SetUint(uintValue)
		return theValue.Elem(), err
	}

	// Floating point numbers
	if strings.HasPrefix(theType.String(), "float") {
		floatValue, err := getFloatValue(theType.String(), value)
		if err != nil {
			return reflect.Value{}, err
		}

		theValue.Elem().SetFloat(floatValue)
		return theValue.Elem(), err
	}

	// Strings
	if theType.String() == "string" {
		stringValue, err := getStringValue(theType.String(), value)
		if err != nil {
			return reflect.Value{}, err
		}

		theValue.Elem().SetString(stringValue)
		return theValue.Elem(), nil
	}

	return reflect.Value{}, NewErrUnsupportedType(valueType)
}
```

5、slice类型处理<br>
```golang
// reflectValues converts interface{} to reflect.Value based on string type
// representing a slice of values
func reflectValues(valueType string, value interface{}) (reflect.Value, error) {
	theType, ok := typesMap[valueType]
	if !ok {
		return reflect.Value{}, NewErrUnsupportedType(valueType)
	}

	// For NULL we return an empty slice
	if value == nil {
		return reflect.MakeSlice(theType, 0, 0), nil
	}

	var theValue reflect.Value

	// Booleans
	if theType.String() == "[]bool" {
		bools := reflect.ValueOf(value)

		theValue = reflect.MakeSlice(theType, bools.Len(), bools.Len())
		for i := 0; i < bools.Len(); i++ {
			boolValue, err := getBoolValue(strings.Split(theType.String(), "[]")[1], bools.Index(i).Interface())
			if err != nil {
				return reflect.Value{}, err
			}

			theValue.Index(i).SetBool(boolValue)
		}

		return theValue, nil
	}

	// Integers
	if strings.HasPrefix(theType.String(), "[]int") {
		ints := reflect.ValueOf(value)

		theValue = reflect.MakeSlice(theType, ints.Len(), ints.Len())
		for i := 0; i < ints.Len(); i++ {
			intValue, err := getIntValue(strings.Split(theType.String(), "[]")[1], ints.Index(i).Interface())
			if err != nil {
				return reflect.Value{}, err
			}

			theValue.Index(i).SetInt(intValue)
		}

		return theValue, nil
	}

	// Unsigned integers
	if strings.HasPrefix(theType.String(), "[]uint") || theType.String() == "[]byte" {

		// Decode the base64 string if the value type is []uint8 or it's alias []byte
		// See: https://golang.org/pkg/encoding/json/#Marshal
		// > Array and slice values encode as JSON arrays, except that []byte encodes as a base64-encoded string
		if reflect.TypeOf(value).String() == "string" {
			output, err := base64.StdEncoding.DecodeString(value.(string))
			if err != nil {
				return reflect.Value{}, err
			}
			value = output
		}

		uints := reflect.ValueOf(value)

		theValue = reflect.MakeSlice(theType, uints.Len(), uints.Len())
		for i := 0; i < uints.Len(); i++ {
			uintValue, err := getUintValue(strings.Split(theType.String(), "[]")[1], uints.Index(i).Interface())
			if err != nil {
				return reflect.Value{}, err
			}

			theValue.Index(i).SetUint(uintValue)
		}

		return theValue, nil
	}

	// Floating point numbers
	if strings.HasPrefix(theType.String(), "[]float") {
		floats := reflect.ValueOf(value)

		theValue = reflect.MakeSlice(theType, floats.Len(), floats.Len())
		for i := 0; i < floats.Len(); i++ {
			floatValue, err := getFloatValue(strings.Split(theType.String(), "[]")[1], floats.Index(i).Interface())
			if err != nil {
				return reflect.Value{}, err
			}

			theValue.Index(i).SetFloat(floatValue)
		}

		return theValue, nil
	}

	// Strings
	if theType.String() == "[]string" {
		strs := reflect.ValueOf(value)

		theValue = reflect.MakeSlice(theType, strs.Len(), strs.Len())
		for i := 0; i < strs.Len(); i++ {
			strValue, err := getStringValue(strings.Split(theType.String(), "[]")[1], strs.Index(i).Interface())
			if err != nil {
				return reflect.Value{}, err
			}

			theValue.Index(i).SetString(strValue)
		}

		return theValue, nil
	}

	return reflect.Value{}, NewErrUnsupportedType(valueType)
}
```

##	Broker
初始化Server时会通过配置文件来建立不同的Broker

```GOLANG
// Broker - a common interface for all brokers
type Broker interface {
	GetConfig() *config.Config
	SetRegisteredTaskNames(names []string)
	IsTaskRegistered(name string) bool

	//Broker 启动一个任务消费者
	StartConsuming(consumerTag string, concurrency int, p TaskProcessor) (bool, error)
	StopConsuming()
	Publish(ctx context.Context, task *tasks.Signature) error
	GetPendingTasks(queue string) ([]*tasks.Signature, error)
	GetDelayedTasks() ([]*tasks.Signature, error)
	AdjustRoutingKey(s *tasks.Signature)
}
```


##	Backend
Backend 的目的是把任务的一系列执行状态和结构记录下来，通过任务唯一id：`Signature.UUID`来关联，记录在redis中的结构是以`signature.UUID`为Key，在执行某一个`Signature`任务时会同步的记录一个对应`UUID`的`TaskState`（即任务状态）

GO
```GOLANG
// Backend - a common interface for all result backends
type Backend interface {
	// Group related functions
	InitGroup(groupUUID string, taskUUIDs []string) error
	GroupCompleted(groupUUID string, groupTaskCount int) (bool, error)
	GroupTaskStates(groupUUID string, groupTaskCount int) ([]*tasks.TaskState, error)
	TriggerChord(groupUUID string) (bool, error)

	// Setting / getting task state
	SetStatePending(signature *tasks.Signature) error
	SetStateReceived(signature *tasks.Signature) error
	SetStateStarted(signature *tasks.Signature) error
	SetStateRetry(signature *tasks.Signature) error
	SetStateSuccess(signature *tasks.Signature, results []*tasks.TaskResult) error
	SetStateFailure(signature *tasks.Signature, err string) error
	GetState(taskUUID string) (*tasks.TaskState, error)

	// Purging stored stored tasks states and group meta data
	IsAMQP() bool
	PurgeState(taskUUID string) error
	PurgeGroupMeta(groupUUID string) error
}
```



##	任务编排
Machinery一共提供了三种任务编排方式：

-	Group ： 执行一组异步任务，任务之间互不影响
-	Chain：执行一组同步任务，任务有次序之分，上个任务的出参可作为下个任务的入参
-	Chord：执行一组同步任务，执行完成后，在调用一个回调函数


####	Group
适用场景：需要并发异步执行多个互不影响的任务就可以使用`Group`进行任务编排，类似于一个`sync.WaitGroup`
