package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const subreaperEnvironment = "GO_CLI_LINUX_SUBREAPER"
const subreaperPrivatePrefix = "GO_CLI_PRIVATE_REAPER_"

type subreaperPacket struct {
	Phase string `json:"phase"`
	Nonce string `json:"nonce"`
	PID   int    `json:"pid"`
}

func validateSubreaperPacket(data []byte, phase, nonce string, pid int) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errors.New("bootstrap packet must be an object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read bootstrap packet key: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] || (key != "phase" && key != "nonce" && key != "pid") {
			return errors.New("duplicate or unknown bootstrap packet key")
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("read bootstrap packet value: %w", err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close bootstrap packet: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing bootstrap packet")
	}
	var packet subreaperPacket
	if err := json.Unmarshal(data, &packet); err != nil {
		return fmt.Errorf("decode bootstrap packet: %w", err)
	}
	if len(seen) != 3 || packet.Phase != phase || packet.Nonce != nonce || packet.PID != pid {
		return errors.New("bootstrap phase, nonce or identity mismatch")
	}
	return nil
}

func parseStandaloneSubreaperMode(value string) (bool, error) {
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("GO_CLI_LINUX_SUBREAPER must be 0 or 1")
	}
}

func readSubreaperPrivateEnvironment(env []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, subreaperPrivatePrefix) {
			continue
		}
		name := strings.TrimPrefix(key, subreaperPrivatePrefix)
		switch name {
		case "ROLE", "NONCE", "PARENT", "EXE":
		default:
			return nil, errors.New("unknown private subreaper environment key")
		}
		if _, exists := result[name]; exists || value == "" {
			return nil, errors.New("duplicate or empty private subreaper environment key")
		}
		result[name] = value
	}
	if len(result) != 0 && (len(result) != 4 || result["ROLE"] != "inner") {
		return nil, errors.New("incomplete private subreaper bootstrap")
	}
	return result, nil
}

func stripSubreaperPrivateEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, subreaperPrivatePrefix) {
			result = append(result, entry)
		}
	}
	return result
}

type subreaperProcessIdentity struct {
	pid     int
	parent  int
	state   byte
	started uint64
}

func parseSubreaperProcStat(stat string) (subreaperProcessIdentity, error) {
	var identity subreaperProcessIdentity
	left, right := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if left < 1 || right <= left {
		return identity, errors.New("malformed proc stat command")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(stat[:left]))
	if err != nil || pid <= 0 {
		return identity, errors.New("malformed proc stat PID")
	}
	fields := strings.Fields(stat[right+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return identity, errors.New("incomplete proc stat identity")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return identity, errors.New("malformed proc stat PPID")
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || started == 0 {
		return identity, errors.New("malformed proc stat start time")
	}
	return subreaperProcessIdentity{pid: pid, parent: parent, state: fields[0][0], started: started}, nil
}

func subreaperFailure(err error) int {
	// No configuration, tracing or model initialization has happened here.
	return writeSubreaperFailure(fmt.Errorf("standalone Linux supervisor: %w", err))
}
