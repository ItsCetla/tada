package shellconfig

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOption_Bool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug true
option progress false
option web-tools true
option update-check false`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
	require.Equal(t, true, opts["enable_web_tools"])
	require.Equal(t, false, opts["update_check"])
}

func TestOption_BoolCaseInsensitive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug TRUE
option progress False
option auto-summarize YES`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["progress"])
	require.Equal(t, false, opts["disable_auto_summarize"])
}

func TestOption_String(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option data-directory .tada
option notifications osc`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, ".tada", opts["data_directory"])
	require.Equal(t, "osc", opts["notifications"])
}

func TestOption_List(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option context-path .cursorrules
option context-path TADA.md`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["context_paths"].([]any)
	require.Len(t, paths, 2)
	require.Equal(t, ".cursorrules", paths[0])
	require.Equal(t, "TADA.md", paths[1])
}

func TestOption_Reset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./a
option skill-path ./b
option reset skill-path`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Empty(t, opts["skills_paths"].([]any))
}

func TestOption_ResetThenReadd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option skill-path ./inherited-a
option skill-path ./inherited-b
option reset skill-path
option skill-path ./mine`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	paths := opts["skills_paths"].([]any)
	require.Len(t, paths, 1)
	require.Equal(t, "./mine", paths[0])
}

func TestOption_ResetUnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset bogus-key`
	path := filepath.Join(dir, "tadarc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_ResetNonListKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option reset debug`
	path := filepath.Join(dir, "tadarc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not one")
}

func TestOption_UIUnknownKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui bogus true`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_UIExitBanner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui exit-banner compact`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, "compact", ui["exit_banner"])

	_, err = LoadShellConfig(t.Context(), path, []byte(`option ui exit-banner bogus`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects default, compact, or none")
}

func TestOption_UIDiff(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	for _, mode := range []string{"auto", "unified", "split"} {
		jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui diff `+mode))
		require.NoError(t, err)

		var result map[string]any
		require.NoError(t, json.Unmarshal(jsonBytes, &result))

		ui := result["options"].(map[string]any)["tui"].(map[string]any)
		require.Equal(t, mode, ui["diff_mode"])
	}

	_, err := LoadShellConfig(t.Context(), path, []byte(`option ui diff sideways`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects auto, unified, or split")
}

func TestOption_BoolShorthand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug
option auto-summarize`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.Equal(t, false, opts["disable_auto_summarize"])
}

func TestOption_InvertedBool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option auto-summarize false`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["disable_auto_summarize"])
}

// TestOption_ObsoleteMetrics verifies that the removed "metrics" option is
// accepted and ignored, so configs that turned telemetry off keep loading.
func TestOption_ObsoleteMetrics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option debug
option metrics false`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["debug"])
	require.NotContains(t, opts, "disable_metrics")
}

func TestOption_UnknownKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option bogus-key value`
	path := filepath.Join(dir, "tadarc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown key")
}

func TestOption_RequestTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option request-timeout 300
option request-timeout 0`
	path := filepath.Join(dir, "tadarc")

	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, float64(0), opts["request_timeout"])
}

func TestOption_RequestTimeoutInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := `option request-timeout soon`
	path := filepath.Join(dir, "tadarc")

	_, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects a number")
}

func TestOption_CompactionTokens(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		script  string
		key     string
		want    float64
		wantErr bool
	}{
		{"reserve", "option compaction-reserve-tokens 32768", "compaction_reserve_tokens", 32768, false},
		{"keep recent", "option compaction-keep-recent-tokens 40000", "compaction_keep_recent_tokens", 40000, false},
		{"not a number", "option compaction-keep-recent-tokens lots", "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "tadarc")
			jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(tc.script))
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "expects a number")
				return
			}
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal(jsonBytes, &result))
			require.Equal(t, tc.want, result["options"].(map[string]any)[tc.key])
		})
	}
}

func TestOption_Sandbox(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	script := "option sandbox workspace-write\noption sandbox-network on"
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, "workspace-write", opts["sandbox"])
	require.Equal(t, true, opts["sandbox_network"])
}

func TestOption_SandboxInvalid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option sandbox on`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "expects one of off, read-only, workspace-write")
}

func TestOption_UIIntro(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui intro false`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, false, ui["intro"])
}

func TestOption_UIAmbient(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui ambient off`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.Equal(t, false, ui["ambient"])
}

func TestOption_MagicContext(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	script := `option magic-context
option magic-context-threshold 70`
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(script))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	mc := result["options"].(map[string]any)["magic_context"].(map[string]any)
	require.Equal(t, true, mc["enabled"])
	require.Equal(t, float64(70), mc["execute_threshold_percentage"])
}

func TestOption_MagicContextOff(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option magic-context off`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	mc := result["options"].(map[string]any)["magic_context"].(map[string]any)
	require.Equal(t, false, mc["enabled"])
}

func TestOption_MagicContextThresholdRange(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	_, err := LoadShellConfig(t.Context(), path, []byte(`option magic-context-threshold 95`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "20 to 90")
}

func TestOption_UITextWidth(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(`option ui text-width 120`))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	ui := result["options"].(map[string]any)["tui"].(map[string]any)
	require.EqualValues(t, 120, ui["text_width"])

	for _, bad := range []string{"-1", "wide"} {
		_, err = LoadShellConfig(t.Context(), path, []byte(`option ui text-width `+bad))
		require.Error(t, err)
		require.Contains(t, err.Error(), "expects a non-negative integer")
	}
}

func TestOption_ClaudeCodeStrictMCP(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte("option claude-code-strict-mcp true"))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, true, opts["claude_code_strict_mcp"])
}

func TestOption_ToolProfile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "tadarc")
	src := "option tool-profile lean\noption enable-tool lsp_rename\noption enable-tool lsp_replace_symbol"
	jsonBytes, err := LoadShellConfig(t.Context(), path, []byte(src))
	require.NoError(t, err)

	var result map[string]any
	require.NoError(t, json.Unmarshal(jsonBytes, &result))

	opts := result["options"].(map[string]any)
	require.Equal(t, "lean", opts["tool_profile"])
	require.Equal(t, []any{"lsp_rename", "lsp_replace_symbol"}, opts["enabled_tools"])

	_, err = LoadShellConfig(t.Context(), path, []byte("option tool-profile tiny"))
	require.Error(t, err)
}
