package mongo

import (
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestIsUniqueViolation(t *testing.T) {
	dup := mongo.WriteException{
		WriteErrors: mongo.WriteErrors{{Code: 11000, Message: "E11000 duplicate key error"}},
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"duplicate key", dup, true},
		{"wrapped duplicate key", fmt.Errorf("create agent: %w", dup), true},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUniqueViolation(tt.err); got != tt.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
