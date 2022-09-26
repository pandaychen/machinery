package tasks

//任务编排（多个任务）

import (
	"fmt"

	"github.com/google/uuid"
)

// Chain creates a chain of tasks to be executed one after another
type Chain struct {
	Tasks []*Signature
}

// Group creates a set of tasks to be executed in parallel
// 分组任务的定义
type Group struct {
	GroupUUID string       //组ID
	Tasks     []*Signature //多个signature
}

// Chord adds an optional callback to the group to be executed
// after all tasks in the group finished

// Chord任务是在Group任务的基础上加入了一个Callback单任务，其编排的目的就是在所有Group的任务做成功后，进行一个回调函数
type Chord struct {
	Group    *Group
	Callback *Signature //
}

// GetUUIDs returns slice of task UUIDS
func (group *Group) GetUUIDs() []string {
	taskUUIDs := make([]string, len(group.Tasks))
	for i, signature := range group.Tasks {
		taskUUIDs[i] = signature.UUID
	}
	return taskUUIDs
}

// NewChain creates a new chain of tasks to be processed one by one, passing
// results unless task signatures are set to be immutable
func NewChain(signatures ...*Signature) (*Chain, error) {
	// Auto generate task UUIDs if needed
	for _, signature := range signatures {
		if signature.UUID == "" {
			signatureID := uuid.New().String()
			signature.UUID = fmt.Sprintf("task_%v", signatureID)
		}
	}

	for i := len(signatures) - 1; i > 0; i-- {
		if i > 0 {
			signatures[i-1].OnSuccess = []*Signature{signatures[i]}
		}
	}

	chain := &Chain{Tasks: signatures}

	return chain, nil
}

// NewGroup creates a new group of tasks to be processed in parallel
// 创建group任务
func NewGroup(signatures ...*Signature) (*Group, error) {
	// Generate a group UUID
	groupUUID := uuid.New().String()
	groupID := fmt.Sprintf("group_%v", groupUUID)

	// Auto generate task UUIDs if needed, group tasks by common group UUID
	for _, signature := range signatures {
		if signature.UUID == "" {
			signatureID := uuid.New().String()
			signature.UUID = fmt.Sprintf("task_%v", signatureID)
		}
		signature.GroupUUID = groupID
		signature.GroupTaskCount = len(signatures)
	}

	return &Group{
		GroupUUID: groupID,
		Tasks:     signatures,
	}, nil
}

// NewChord creates a new chord (a group of tasks with a single callback
// to be executed after all tasks in the group has completed)
func NewChord(group *Group, callback *Signature) (*Chord, error) {
	if callback.UUID == "" {
		// Generate a UUID for the chord callback
		callbackUUID := uuid.New().String()
		callback.UUID = fmt.Sprintf("chord_%v", callbackUUID)
	}

	// Add a chord callback to all tasks
	for _, signature := range group.Tasks {
		signature.ChordCallback = callback
	}

	return &Chord{Group: group, Callback: callback}, nil
}
