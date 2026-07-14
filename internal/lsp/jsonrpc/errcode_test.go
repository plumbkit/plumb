package jsonrpc

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsMethodNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"raised MethodNotFoundError", &MethodNotFoundError{Method: "textDocument/diagnostic"}, true},
		{"wrapped MethodNotFoundError", fmt.Errorf("gopls diagnostic: %w", &MethodNotFoundError{Method: "x"}), true},
		{"server -32601 response", &wireError{Code: errCodeMethodNotFound, Message: "unsupported"}, true},
		{"wrapped server -32601", fmt.Errorf("gopls diagnostic: %w", &wireError{Code: errCodeMethodNotFound, Message: "no"}), true},
		{"server internal error", &wireError{Code: errCodeInternal, Message: "boom"}, false},
		{"unrelated error", errors.New("timeout"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMethodNotFound(tt.err); got != tt.want {
				t.Errorf("IsMethodNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
