package pipeline

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common/atomic"
	"github.com/elastic/beats/v7/libbeat/publisher/queue"
	"github.com/elastic/beats/v7/libbeat/tests/resources"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

func TestTiagoPipelineAccepts66000Clients(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping because short is enabled")
	}

	routinesChecker := resources.NewGoroutinesChecker()
	defer routinesChecker.Check(t)

	pipeline := makePipeline(t, Settings{}, makeDiscardQueue())

	defer pipeline.Close()
	ctx, cancel := context.WithCancel(context.Background())

	n := 16 //35000
	clients := []beat.Client{}
	for i := 0; i < n; i++ {
		c, err := pipeline.ConnectWith(beat.ClientConfig{
			CloseRef: ctx,
		})
		if err != nil {
			t.Fatalf("Could not connect to pipeline: %s", err)
		}
		clients = append(clients, c)
	}

	// var wg sync.WaitGroup

	// allPublished := make(chan struct{})
	// wg.Add(1)
	// go func() {
	// 	defer func() {
	// 		close(allPublished)
	// 	}()
	// 	defer func() {
	// 		wg.Done()
	// 	}()

	// 	for i, c := range clients {
	// 		c.Publish(beat.Event{
	// 			Fields: mapstr.M{
	// 				"count": i,
	// 			},
	// 		})
	// 	}
	// }()

	// // fmt.Println("+++++ Waiting for events to be published")
	// <-allPublished
	// // fmt.Println("+++++ All events published, cancelling")
	for i, c := range clients {
		c.Publish(beat.Event{
			Fields: mapstr.M{
				"count": i,
			},
		})
	}

	// Close the first 105 clients
	nn := 6
	tmpC := clients[:n]
	clients = clients[nn:]

	fmt.Println("++++++++++ Closing some workers")
	for _, c := range tmpC {
		c.Close()
	}
	fmt.Println("++++++++++ Closing some workers DONE")
	//time.Sleep(time.Second / 2)
	runtime.Gosched()
	runtime.Gosched()

	cancel()

	// fmt.Println("+++++ Waiting WG")
	// wg.Wait()
	// fmt.Println("+++++ Waiting WG DONE")

	// Make sure all clients are closed
	for _, c := range clients {
		c.Close()
	}
}

// makeDiscardQueue returns a queue that always discards all events
// the producers are assigned an unique incremental ID, when their
// close method is called, this ID is returned
func makeDiscardQueue() queue.Queue {
	var wg sync.WaitGroup
	producerID := atomic.NewInt(0)

	return &testQueue{
		close: func() error {
			//  Wait for all producers to finish
			wg.Wait()
			return nil
		},
		get: func(count int) (queue.Batch, error) {
			return nil, nil
		},

		producer: func(cfg queue.ProducerConfig) queue.Producer {
			producerID.Inc()
			id := producerID.Load()

			// count is a counter that increments on every published event
			// it's also the returned Event ID
			count := uint64(0)
			var producer *testProducer
			producer = &testProducer{
				publish: func(try bool, event interface{}) (queue.EntryID, bool) {
					count++
					return queue.EntryID(count), true
				},
				cancel: func() int {

					wg.Done()
					return id
				},
			}

			wg.Add(1)
			return producer
		},
	}
}
