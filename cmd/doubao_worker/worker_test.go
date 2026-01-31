package main

import (
	"context"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func Test_askDeepSeekForAction(t *testing.T) {
	type args struct {
		ctx    context.Context
		client *openai.Client
		text   string
	}
	tests := []struct {
		name string
		args args
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			askDeepSeekForAction(tt.args.ctx, tt.args.client, tt.args.text)
		})
	}
}
