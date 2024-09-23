package libotel

import (
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common/reload"
	"github.com/elastic/beats/v7/libbeat/outputs"
)

type Pipeline struct{}

func (p Pipeline) Connect() (beat.Client, error) {
	return nil, nil
}

func (p Pipeline) ConnectWith(beat.ClientConfig) (beat.Client, error) {
	return nil, nil
}

func NewOutputFactory() func(outputs.Observer) (string, outputs.Group, error) {
	return nil
}

type OutputReloader struct{}

func (o OutputReloader) Reload(config *reload.ConfigWithMeta) error {
	return nil
}

type Client struct{}

func (c Client) Publish(beat.Event) {}

func (c Client) PublishAll([]beat.Event) {}

func (c Client) Close() error {
	return nil
}

func NewPipeline() beat.Pipeline {
	return Pipeline{}
}
