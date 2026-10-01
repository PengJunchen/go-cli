package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRegisterSkillToolsReportsBoundedPathFreeLoadSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillDir := filepath.Join(home, ".config", "go-cli", "skills")
	require.NoError(t, os.MkdirAll(skillDir, 0o700))
	writeProjectSkill(t, filepath.Join(skillDir, "good.md"), projectSkillContent("good", "GOOD_BODY_CANARY"))
	writeProjectSkill(t, filepath.Join(skillDir, "bad\nname.md"), "RAW_ERROR_CANARY")

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	infos := registerSkillTools(context.Background(), &config.Config{Skill: config.SkillConfig{Dir: skillDir}}, registry)
	require.Len(t, infos, 1)
	assert.Equal(t, "good", infos[0].Name)
	registered, err := registry.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, registered, 1)

	records := skillLoadLogRecords(t, logs.Bytes())
	aggregates := skillLoadRecordsByMessage(records, "assemble_skills_load_report")
	require.Len(t, aggregates, 1)
	aggregate := aggregates[0]
	assert.Equal(t, "WARN", aggregate["level"])
	assert.Equal(t, float64(1), aggregate["loaded"])
	assert.Equal(t, float64(1), aggregate["skipped"])
	assert.Equal(t, "frontmatter=1", aggregate["error_classes"])
	assert.Equal(t, map[string]any{
		"time": aggregate["time"], "level": "WARN", "msg": "assemble_skills_load_report",
		"loaded": float64(1), "skipped": float64(1), "error_classes": "frontmatter=1",
	}, aggregate, "aggregate reports contain only counts and fixed error classes")
	assert.NotContains(t, string(mustMarshalJSON(t, aggregate)), skillDir)
	assert.NotContains(t, string(mustMarshalJSON(t, aggregate)), "bad\\nname.md")
	assert.NotContains(t, string(mustMarshalJSON(t, aggregate)), "RAW_ERROR_CANARY")
	assert.NotContains(t, string(mustMarshalJSON(t, aggregate)), "GOOD_BODY_CANARY")

	diagnostics := skillLoadRecordsByMessage(records, "assemble_skill_load_skipped")
	require.Len(t, diagnostics, 1)
	assert.Equal(t, `skill:"bad\nname.md"`, diagnostics[0]["source_id"])
	assert.Equal(t, "frontmatter", diagnostics[0]["error_class"])
	assert.Equal(t, map[string]any{
		"time": diagnostics[0]["time"], "level": "WARN", "msg": "assemble_skill_load_skipped",
		"source_id": `skill:"bad\nname.md"`, "error_class": "frontmatter",
	}, diagnostics[0], "per-file reports contain only a bounded source id and fixed error class")

	registeredEvents := skillLoadRecordsByMessage(records, "assemble_skills_registered")
	require.Len(t, registeredEvents, 1)
	assert.Equal(t, float64(1), registeredEvents[0]["count"], "successful registrations retain their separate count")
}

func TestRegisterSkillToolsDoesNotReportWhenEverySkillLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillDir := filepath.Join(home, ".config", "go-cli", "skills")
	writeProjectSkill(t, filepath.Join(skillDir, "good.md"), projectSkillContent("good", "good body"))

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())

	require.Len(t, registerSkillTools(context.Background(), &config.Config{Skill: config.SkillConfig{Dir: skillDir}}, registry), 1)
	records := skillLoadLogRecords(t, logs.Bytes())
	assert.Empty(t, skillLoadRecordsByMessage(records, "assemble_skills_load_report"))
	assert.Empty(t, skillLoadRecordsByMessage(records, "assemble_skill_load_skipped"))
}

func TestRegisterSkillToolsDoesNotReportFatalLoadAsSkippedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	skillDir := filepath.Join(home, ".config", "go-cli", "skills")
	writeProjectSkill(t, filepath.Join(skillDir, "bad.md"), "malformed")

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())

	assert.Empty(t, registerSkillTools(ctx, &config.Config{Skill: config.SkillConfig{Dir: skillDir}}, registry))
	records := skillLoadLogRecords(t, logs.Bytes())
	assert.Empty(t, skillLoadRecordsByMessage(records, "assemble_skills_load_report"))
	assert.Empty(t, skillLoadRecordsByMessage(records, "assemble_skill_load_skipped"))
}

func TestProjectSkillLoadReportUsesVerifiedCapturedBytes(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, ".go-cli", "skills")
	writeProjectSkill(t, filepath.Join(skillDir, "good.md"), projectSkillContent("good", "captured good body"))
	badPath := filepath.Join(skillDir, "nested", "bad.md")
	writeProjectSkill(t, badPath, "CAPTURED_RAW_ERROR_CANARY")
	writeProjectSkill(t, filepath.Join(root, ".go-cli.yaml"), "skill:\n  dir: .go-cli/skills\n")
	t.Chdir(root)
	t.Setenv("HOME", t.TempDir())

	ctx := context.Background()
	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	uncurated, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	require.NotNil(t, snapshot)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	trusted, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, trusted.VerifiedProjectSources())

	writeProjectSkill(t, badPath, projectSkillContent("changed", "CHANGED_DISK_CANARY"))
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	infos := registerSkillTools(ctx, trusted, registry)
	require.Len(t, infos, 1, "the malformed captured bytes must stay skipped after the working tree changes")
	assert.Equal(t, "good", infos[0].Name)

	records := skillLoadLogRecords(t, logs.Bytes())
	aggregates := skillLoadRecordsByMessage(records, "assemble_skills_load_report")
	require.Len(t, aggregates, 1)
	assert.Equal(t, float64(1), aggregates[0]["loaded"])
	assert.Equal(t, float64(1), aggregates[0]["skipped"])
	diagnostics := skillLoadRecordsByMessage(records, "assemble_skill_load_skipped")
	require.Len(t, diagnostics, 1)
	assert.Equal(t, `skill:"nested/bad.md"`, diagnostics[0]["source_id"])
	assert.Equal(t, "frontmatter", diagnostics[0]["error_class"])
	newEvents, err := json.Marshal(append(aggregates, diagnostics...))
	require.NoError(t, err)
	assert.NotContains(t, string(newEvents), root)
	assert.NotContains(t, string(newEvents), "CAPTURED_RAW_ERROR_CANARY")
	assert.NotContains(t, string(newEvents), "CHANGED_DISK_CANARY")
}

func skillLoadLogRecords(t *testing.T, output []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		records = append(records, record)
	}
	return records
}

func skillLoadRecordsByMessage(records []map[string]any, message string) []map[string]any {
	var matches []map[string]any
	for _, record := range records {
		if record["msg"] == message {
			matches = append(matches, record)
		}
	}
	return matches
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}
