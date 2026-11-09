package jobqueue

import (
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

func TestTaskIDRoundTrip(t *testing.T) {
	job := id.New()
	for _, nodeID := range []string{"backup", "store_master", "a-b_c"} {
		taskID := TaskID(job, nodeID, 3)
		gotJob, gotNode, gotAttempt, err := ParseTaskID(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if gotJob != job || gotNode != nodeID || gotAttempt != 3 {
			t.Fatalf("round trip of %s gave %s %s %d", taskID, gotJob, gotNode, gotAttempt)
		}
	}
	for _, bad := range []string{"", "x", "x.y", "x.y.z", "NOTAULID.node.1"} {
		if _, _, _, err := ParseTaskID(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestNames(t *testing.T) {
	if StreamKey(node.PoolMedia) != "vidyom:tasks:media" || GroupName(node.PoolAI) != "vidyom-ai" {
		t.Fatal("stream names changed")
	}
}
