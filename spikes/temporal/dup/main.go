package main

import (
	"context"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

func main() {
	c, err := client.Dial(client.Options{HostPort: "localhost:7233"})
	if err != nil {
		panic(err)
	}
	defer c.Close()
	id := fmt.Sprintf("dupspike-%d", time.Now().UnixNano())
	for i := 0; i < 3; i++ {
		_, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{ID: id, TaskQueue: "nobody-polls-this",
			WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
			WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, "Nothing")
		fmt.Println("start", i, "err:", err)
	}
	it := c.GetWorkflowHistory(context.Background(), id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for it.HasNext() {
		ev, _ := it.Next()
		fmt.Println(ev.GetEventId(), ev.GetEventType())
	}
}
