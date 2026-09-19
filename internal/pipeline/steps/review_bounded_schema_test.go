package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewStep_SelectsBoundedOutputSchemaOnlyForBoundedStrategy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		strategy       string
		wantSchema     json.RawMessage
		wantPromptKeys bool
	}{
		{name: "iterative", strategy: config.ReviewStrategyIterative, wantSchema: reviewFindingsSchema},
		{name: "bounded", strategy: config.ReviewStrategyBounded, wantSchema: boundedReviewFindingsSchema, wantPromptKeys: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(cleanReviewJSON)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Review.Strategy = tc.strategy

			if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("review calls = %d, want 1", len(ag.calls))
			}
			if !bytes.Equal(ag.calls[0].JSONSchema, tc.wantSchema) {
				t.Fatalf("schema passed to Review = %s, want %s", ag.calls[0].JSONSchema, tc.wantSchema)
			}
			hasPromptKeys := strings.Contains(ag.calls[0].Prompt, `three separate JSON fields: "id"`) &&
				strings.Contains(ag.calls[0].Prompt, `"evidence"`) &&
				strings.Contains(ag.calls[0].Prompt, `"verification"`)
			if hasPromptKeys != tc.wantPromptKeys {
				t.Fatalf("bounded JSON-field prompt present = %t, want %t", hasPromptKeys, tc.wantPromptKeys)
			}
		})
	}
}

func TestBoundedReviewFindingsSchema_PreservesReviewContractAndRequiresMachineFields(t *testing.T) {
	t.Parallel()
	ordinary := decodeSchemaObject(t, reviewFindingsSchema)
	bounded := decodeSchemaObject(t, boundedReviewFindingsSchema)

	ordinaryProperties := schemaObject(t, ordinary, "properties")
	boundedProperties := schemaObject(t, bounded, "properties")
	if !reflect.DeepEqual(ordinaryProperties["reviewed_paths"], boundedProperties["reviewed_paths"]) {
		t.Fatal("bounded schema changed the reviewed_paths contract")
	}
	if !reflect.DeepEqual(ordinary["required"], bounded["required"]) {
		t.Fatal("bounded schema changed the top-level required fields")
	}

	ordinaryItem := reviewFindingItemSchema(t, ordinary)
	boundedItem := reviewFindingItemSchema(t, bounded)
	ordinaryItemProperties := schemaObject(t, ordinaryItem, "properties")
	boundedItemProperties := schemaObject(t, boundedItem, "properties")
	for _, field := range []string{"evidence", "verification"} {
		if _, ok := ordinaryItemProperties[field]; ok {
			t.Fatalf("iterative Review schema unexpectedly defines %q", field)
		}
		property, ok := boundedItemProperties[field].(map[string]any)
		if !ok || property["type"] != "string" {
			t.Fatalf("bounded Review field %q = %#v, want string schema", field, boundedItemProperties[field])
		}
	}
	ordinaryRequired := schemaStringSet(t, ordinaryItem, "required")
	boundedRequired := schemaStringSet(t, boundedItem, "required")
	if ordinaryRequired["id"] {
		t.Fatal("iterative Review schema must keep id optional")
	}
	for _, field := range []string{"id", "evidence", "verification"} {
		if !boundedRequired[field] {
			t.Fatalf("bounded Review schema does not require %q", field)
		}
	}
}

func TestReviewStep_BoundedCodexSchemaBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake Codex process is a POSIX shell fixture")
	}

	const oldFinding = `{"findings":[{"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"source evidence and verification were folded into prose","action":"auto-fix","review_scope":"source"}],"risk_level":"high","risk_rationale":"one defect","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`
	const completeFinding = `{"findings":[{"id":"review-1","severity":"error","file":"feature.txt","line":1,"description":"one defect","evidence":"feature.txt:1 reaches the failing branch","verification":"go test ./internal/pipeline/steps -run TestFocused","action":"auto-fix","review_scope":"source"}],"risk_level":"high","risk_rationale":"one defect","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`
	const clean = `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`

	for _, tc := range []struct {
		name         string
		payload      string
		wantError    string
		wantApproval bool
		wantFindings int
	}{
		{
			name:      "old payload without separate fields is rejected",
			payload:   oldFinding,
			wantError: `missing required field "evidence"`,
		},
		{
			name:         "same finding with separate fields reaches bounded adjudication",
			payload:      completeFinding,
			wantApproval: true,
			wantFindings: 1,
		},
		{
			name:         "zero findings still masks per-finding requirements",
			payload:      clean,
			wantFindings: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			fakeDir := t.TempDir()
			schemaPath := filepath.Join(fakeDir, "normalized-schema.json")
			eventsPath := filepath.Join(fakeDir, "events.jsonl")
			callsPath := filepath.Join(fakeDir, "calls.txt")
			writeFakeCodexReviewEvents(t, eventsPath, tc.payload)
			bin := filepath.Join(fakeDir, "codex")
			if err := os.WriteFile(bin, []byte(`#!/bin/sh
schema=""
want_schema=""
for arg do
  if [ "$want_schema" = "1" ]; then
    schema="$arg"
    want_schema=""
    continue
  fi
  if [ "$arg" = "--output-schema" ]; then
    want_schema="1"
  fi
done
if [ -z "$schema" ]; then
  echo "missing --output-schema" >&2
  exit 2
fi
cp "$schema" "$FAKE_CODEX_SCHEMA"
printf 'call\n' >> "$FAKE_CODEX_CALLS"
cat >/dev/null
cat "$FAKE_CODEX_EVENTS"
`), 0o755); err != nil {
				t.Fatal(err)
			}
			codex, err := agent.New(types.AgentCodex, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = codex.Close() })

			sctx := newTestContextWithDBRecords(t, codex, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Agent = types.AgentCodex
			sctx.Config.Review.Strategy = config.ReviewStrategyBounded
			sctx.Env = []string{
				"FAKE_CODEX_SCHEMA=" + schemaPath,
				"FAKE_CODEX_EVENTS=" + eventsPath,
				"FAKE_CODEX_CALLS=" + callsPath,
			}

			outcome, err := (&ReviewStep{}).Execute(sctx)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("Execute() error = %v, want %q", err, tc.wantError)
				}
				if outcome != nil {
					t.Fatalf("Execute() outcome = %+v, want nil", outcome)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if outcome.NeedsApproval != tc.wantApproval {
					t.Fatalf("NeedsApproval = %t, want %t", outcome.NeedsApproval, tc.wantApproval)
				}
				findings, parseErr := types.ParseFindingsJSON(outcome.Findings)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				if len(findings.Items) != tc.wantFindings {
					t.Fatalf("findings = %d, want %d", len(findings.Items), tc.wantFindings)
				}
			}

			calls, readErr := os.ReadFile(callsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(calls) != "call\n" {
				t.Fatalf("Codex calls = %q, want one full review invocation", calls)
			}
			normalized, readErr := os.ReadFile(schemaPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			assertNormalizedBoundedReviewSchema(t, normalized)
		})
	}
}

func writeFakeCodexReviewEvents(t *testing.T, path, payload string) {
	t.Helper()
	events := []any{
		map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": payload}},
		map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}},
	}
	var lines []string
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNormalizedBoundedReviewSchema(t *testing.T, raw json.RawMessage) {
	t.Helper()
	schema := decodeSchemaObject(t, raw)
	if schema["additionalProperties"] != false {
		t.Fatal("normalized Review schema is not closed")
	}
	item := reviewFindingItemSchema(t, schema)
	if item["additionalProperties"] != false {
		t.Fatal("normalized Review finding schema is not closed")
	}
	required := schemaStringSet(t, item, "required")
	properties := schemaObject(t, item, "properties")
	if len(required) != len(properties) {
		t.Fatalf("normalized finding required fields = %d, properties = %d", len(required), len(properties))
	}
	for _, field := range []string{"id", "evidence", "verification"} {
		if !required[field] {
			t.Fatalf("normalized bounded Review schema does not require %q", field)
		}
		property := properties[field].(map[string]any)
		if property["type"] != "string" {
			t.Fatalf("normalized bounded Review field %q type = %#v, want non-null string", field, property["type"])
		}
	}
}

func decodeSchemaObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func schemaObject(t *testing.T, schema map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := schema[key].(map[string]any)
	if !ok {
		t.Fatalf("schema field %q = %#v, want object", key, schema[key])
	}
	return value
}

func reviewFindingItemSchema(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	properties := schemaObject(t, schema, "properties")
	findings, ok := properties["findings"].(map[string]any)
	if !ok {
		t.Fatalf("findings schema = %#v, want object", properties["findings"])
	}
	item, ok := findings["items"].(map[string]any)
	if !ok {
		t.Fatalf("finding item schema = %#v, want object", findings["items"])
	}
	return item
}

func schemaStringSet(t *testing.T, schema map[string]any, key string) map[string]bool {
	t.Helper()
	values, ok := schema[key].([]any)
	if !ok {
		t.Fatalf("schema field %q = %#v, want array", key, schema[key])
	}
	set := make(map[string]bool, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("schema field %q contains non-string %#v", key, value)
		}
		set[text] = true
	}
	return set
}
