// Package llama is a small tolerant client for a llama.cpp llama-server.
package llama

import "errors"

// Health is the /health response.
type Health struct{ Status string } // "ok", "loading model", "error"; "" if unknown

// Props is the subset of /props the dashboard uses.
type Props struct {
	TotalSlots, NCtx                             int
	ModelAlias, ModelFtype, ModelPath, BuildInfo string
	IsSleeping, EndpointSlots, EndpointMetrics   bool
}

// Slot is one entry of /slots.
type Slot struct {
	ID, NCtx, TaskID                           int
	IsProcessing, Speculative, Stream          bool
	PromptTokens, PromptProcessed, PromptCache int
	Decoded, Remain                            int // from next_token[0]; Remain -1 = unlimited
	Temperature                                float64
}

// Metrics maps a metric name without the "llamacpp:" prefix to its value.
type Metrics map[string]float64

var (
	ErrDisabled     = errors.New("endpoint disabled (501)")
	ErrUnauthorized = errors.New("unauthorized (401/403)")
	ErrLoading      = errors.New("server loading (503)")
	ErrHTTPStatus   = errors.New("HTTP error status") // any other non-2xx answer
)
