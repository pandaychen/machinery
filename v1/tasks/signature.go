package tasks

import (
	"fmt"
	"time"

	"github.com/RichardKnop/machinery/v1/utils"

	"github.com/google/uuid"
)

// Arg represents a single argument passed to invocation fo a task
type Arg struct {
	Name  string      `bson:"name"`
	Type  string      `bson:"type"`
	Value interface{} `bson:"value"`
}

// Headers represents the headers which should be used to direct the task
type Headers map[string]interface{}

// Set on Headers implements opentracing.TextMapWriter for trace propagation
func (h Headers) Set(key, val string) {
	h[key] = val
}

// ForeachKey on Headers implements opentracing.TextMapReader for trace propagation.
// It is essentially the same as the opentracing.TextMapReader implementation except
// for the added casting from interface{} to string.
func (h Headers) ForeachKey(handler func(key, val string) error) error {
	for k, v := range h {
		// Skip any non string values
		stringValue, ok := v.(string)
		if !ok {
			continue
		}

		if err := handler(k, stringValue); err != nil {
			return err
		}
	}

	return nil
}

// Signature represents a single task invocation

//核心结构：标识单个（原子）任务，同时也是参数传递和结果记录的地方，与整个任务的生命周期都有关
type Signature struct {
	UUID       string     //任务的唯一标识
	Name       string     //注册绑定的处理方法的名字！触发任务时Worker使用Name来获取任务执行函数
	RoutingKey string     //任务会分发到RoutingKey指向的topic队列中被处理！
	ETA        *time.Time //指定任务执行时间

	//GroupUUID和GroupCount：是该任务跟随的任务组ID，和任务组任务总数，分组任务时会用到
	GroupUUID      string
	GroupTaskCount int

	Args []Arg //重要参数：触发执行任务函数的入参，Arg的结构如下

	/*
			// Arg represents a single argument passed to invocation fo a task
		type Arg struct {
			Name  string      `bson:"name"`
			Type  string      `bson:"type"`	//Type就是参数的类型
			Value interface{} `bson:"value"`	//Value就是参数的值
		}
	*/

	Headers       Headers
	Priority      uint8
	Immutable     bool
	RetryCount    int
	RetryTimeout  int
	OnSuccess     []*Signature //成功后的回调
	OnError       []*Signature //失败后的回调
	ChordCallback *Signature   //调用链完成后的回调方法
	//MessageGroupId for Broker, e.g. SQS
	BrokerMessageGroupId string
	//ReceiptHandle of SQS Message
	SQSReceiptHandle string
	// StopTaskDeletionOnError used with sqs when we want to send failed messages to dlq,
	// and don't want machinery to delete from source queue
	StopTaskDeletionOnError bool
	// IgnoreWhenTaskNotRegistered auto removes the request when there is no handeler available
	// When this is true a task with no handler will be ignored and not placed back in the queue
	IgnoreWhenTaskNotRegistered bool
}

// NewSignature creates a new task signature
func NewSignature(name string, args []Arg) (*Signature, error) {
	signatureID := uuid.New().String()
	return &Signature{
		UUID: fmt.Sprintf("task_%v", signatureID),
		Name: name,
		Args: args,
	}, nil
}

func CopySignatures(signatures ...*Signature) []*Signature {
	var sigs = make([]*Signature, len(signatures))
	for index, signature := range signatures {
		sigs[index] = CopySignature(signature)
	}
	return sigs
}

func CopySignature(signature *Signature) *Signature {
	var sig = new(Signature)
	_ = utils.DeepCopy(sig, signature)
	return sig
}
