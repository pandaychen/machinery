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