package llama

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// ParseHealth parses /health. An error body yields an empty Status.
func ParseHealth(b []byte) (Health, error) {
	var r struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Health{}, err
	}
	return Health{Status: r.Status}, nil
}

// ParseProps parses /props.
func ParseProps(b []byte) (Props, error) {
	var r struct {
		TotalSlots      int    `json:"total_slots"`
		ModelAlias      string `json:"model_alias"`
		ModelFtype      string `json:"model_ftype"`
		ModelPath       string `json:"model_path"`
		BuildInfo       string `json:"build_info"`
		IsSleeping      bool   `json:"is_sleeping"`
		EndpointSlots   bool   `json:"endpoint_slots"`
		EndpointMetrics bool   `json:"endpoint_metrics"`
		Default         struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Props{}, err
	}
	return Props{
		TotalSlots: r.TotalSlots, NCtx: r.Default.NCtx,
		ModelAlias: r.ModelAlias, ModelFtype: r.ModelFtype, ModelPath: r.ModelPath,
		BuildInfo: r.BuildInfo, IsSleeping: r.IsSleeping,
		EndpointSlots: r.EndpointSlots, EndpointMetrics: r.EndpointMetrics,
	}, nil
}

type nextToken struct {
	NRemain  int `json:"n_remain"`
	NDecoded int `json:"n_decoded"`
}

// decodeNextToken accepts next_token as an array or a single object; anything else is zero.
func decodeNextToken(raw json.RawMessage) nextToken {
	raw = bytes.TrimSpace(raw)
	var nt nextToken
	switch {
	case len(raw) == 0:
	case raw[0] == '[':
		var arr []nextToken
		if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
			nt = arr[0]
		}
	case raw[0] == '{':
		_ = json.Unmarshal(raw, &nt)
	}
	return nt
}

// ParseSlots parses /slots. Null or empty input yields an empty slice; a non-array is an error.
func ParseSlots(b []byte) ([]Slot, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	var raws []struct {
		ID              int             `json:"id"`
		NCtx            int             `json:"n_ctx"`
		Speculative     bool            `json:"speculative"`
		IsProcessing    bool            `json:"is_processing"`
		TaskID          int             `json:"id_task"`
		PromptTokens    int             `json:"n_prompt_tokens"`
		PromptProcessed int             `json:"n_prompt_tokens_processed"`
		PromptCache     int             `json:"n_prompt_tokens_cache"`
		NextToken       json.RawMessage `json:"next_token"`
		Params          struct {
			Temperature float64 `json:"temperature"`
			Stream      bool    `json:"stream"`
		} `json:"params"`
	}
	if err := json.Unmarshal(b, &raws); err != nil {
		return nil, err
	}
	out := make([]Slot, 0, len(raws))
	for _, r := range raws {
		nt := decodeNextToken(r.NextToken)
		out = append(out, Slot{
			ID: r.ID, NCtx: r.NCtx, TaskID: r.TaskID,
			IsProcessing: r.IsProcessing, Speculative: r.Speculative, Stream: r.Params.Stream,
			PromptTokens: r.PromptTokens, PromptProcessed: r.PromptProcessed, PromptCache: r.PromptCache,
			Decoded: nt.NDecoded, Remain: nt.NRemain, Temperature: r.Params.Temperature,
		})
	}
	return out, nil
}

// ParseMetrics parses Prometheus text. Comments, blanks and malformed lines are skipped;
// names lose the "llamacpp:" prefix and any label set.
func ParseMetrics(b []byte) (Metrics, error) {
	m := Metrics{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexAny(line, " \t")
		if i < 0 {
			continue
		}
		name, val := strings.TrimSpace(line[:i]), line[i+1:]
		if j := strings.IndexByte(name, '{'); j >= 0 {
			name = name[:j]
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		m[strings.TrimPrefix(name, "llamacpp:")] = v
	}
	if err := sc.Err(); err != nil {
		return m, errors.New("metrics: " + err.Error())
	}
	return m, nil
}
