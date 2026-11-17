#!/usr/bin/env bash
# End-to-end smoke test on a local model: build Ta-Da from the working
# tree, drive the real TUI in tmux to build a small Go package, then check
# the result independently (hidden tests, session DB, pane capture, log).
#
# Usage: scripts/e2e-local.sh            (or: task e2e:local)
#
# Environment:
#   TADA_E2E_MACHINE  machine/provider name from the user's tada.json (pc)
#   TADA_E2E_MODEL    model id (default: the machine's configured model)
#   TADA_E2E_TIMEOUT  seconds to wait for the agent to finish (1800)
#   TADA_E2E_OUT      artifacts dir (default: a new temp dir)
#   TADA_E2E_SOURCE   config holding the machine (~/.local/share/tada/tada.json)
#   TADA_E2E_KEEP     1 leaves the tmux session running after the run
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
MACHINE=${TADA_E2E_MACHINE:-pc}
TIMEOUT=${TADA_E2E_TIMEOUT:-1800}
SOURCE=${TADA_E2E_SOURCE:-$HOME/.local/share/tada/tada.json}
OUT=${TADA_E2E_OUT:-$(mktemp -d "${TMPDIR:-/tmp}/tada-e2e.XXXXXX")}
OUT=$(cd "$(mkdir -p "$OUT" && echo "$OUT")" && pwd)
PROJ=$OUT/project
DATA=$OUT/data
DB=$DATA/tada.db
LOG=$DATA/logs/tada.log
PANE=$OUT/pane.txt
FRAMES=$OUT/pane-frames.txt
SCROLL=$OUT/pane-scroll.txt
SOCK=tada-e2e-$$
COLS=120
ROWS=40

FAILS=0
RESULTS=()
step() { printf '\n== %s\n' "$*"; }
pass() { RESULTS+=("PASS  $1"); printf 'PASS  %s\n' "$1"; }
fail() { RESULTS+=("FAIL  $1"); printf 'FAIL  %s\n' "$1"; FAILS=$((FAILS + 1)); }
info() { RESULTS+=("INFO  $1"); printf 'INFO  %s\n' "$1"; }
q() { sqlite3 -readonly "$DB" "$1" 2>/dev/null; }
tmx() { tmux -L "$SOCK" "$@"; }

cleanup() {
	if [ "${TADA_E2E_KEEP:-0}" != 1 ]; then
		tmx kill-server 2>/dev/null || true
	else
		echo "tmux session kept: tmux -L $SOCK attach -t e2e"
	fi
}
trap cleanup EXIT

summary() {
	step "Summary"
	printf '%s\n' "${RESULTS[@]}"
	echo
	echo "Artifacts: $OUT"
	echo "  pane:    $PANE (last screen), $SCROLL (whole chat), $FRAMES (live)"
	echo "  log:     $LOG"
	echo "  db:      $DB"
	echo "  project: $PROJ"
	if [ "$FAILS" -gt 0 ]; then
		echo "RESULT: FAIL ($FAILS check(s) failed)"
		exit 1
	fi
	echo "RESULT: PASS"
}

for tool in go tmux sqlite3 jq curl git; do
	command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 2; }
done

step "Local model ($MACHINE)"
machine=$(jq -e --arg m "$MACHINE" '.machines[$m]' "$SOURCE") || {
	echo "machine '$MACHINE' not found in $SOURCE" >&2
	exit 2
}
URL=$(jq -r '.url' <<<"$machine")
KEY=$(jq -r '.api_key // ""' <<<"$machine")
MODEL=${TADA_E2E_MODEL:-$(jq -r '.model' <<<"$machine")}
if ! models=$(curl -fsS -m 10 -H "Authorization: Bearer $KEY" "$URL/models"); then
	echo "UNREACHABLE: $URL did not answer; not switching models." >&2
	exit 3
fi
if ! jq -e --arg id "$MODEL" '[.data[].id] | index($id)' <<<"$models" >/dev/null; then
	echo "model '$MODEL' not served by $URL" >&2
	exit 3
fi
echo "$MACHINE reachable at $URL, model $MODEL"

step "Build"
mkdir -p "$OUT/bin"
(cd "$REPO" && CGO_ENABLED=0 GOEXPERIMENT=greenteagc go build -o "$OUT/bin/tada" .)
echo "built $OUT/bin/tada"

step "Isolated project and data dirs"
mkdir -p "$PROJ" "$DATA" "$OUT/global-config" "$OUT/global-data" "$OUT/cache"
# Only the one machine is carried over; the user's sessions, other
# providers and settings stay out of the run.
jq -n --arg m "$MACHINE" --arg model "$MODEL" --argjson machine "$machine" '{
	machines: {($m): $machine},
	models: {
		large: {provider: $m, model: $model},
		small: {provider: $m, model: $model}
	},
	options: {sandbox: "workspace-write"}
}' >"$OUT/global-data/tada.json"
chmod 600 "$OUT/global-data/tada.json"
# Skip the "initialize this project?" dialog so the prompt reaches the editor.
touch "$DATA/init"
# Reuse the provider catalog so startup does not depend on the network.
if [ -f "$HOME/.local/share/tada/providers.json" ]; then
	cp "$HOME/.local/share/tada/providers.json" "$OUT/global-data/"
fi
(
	cd "$PROJ"
	git init -q
	printf 'module example.com/slugdemo\n\ngo 1.22\n' >go.mod
	git add go.mod
	git -c user.name=e2e -c user.email=e2e@example.com commit -qm init
)
cat >"$OUT/run-tada.sh" <<EOF
#!/usr/bin/env bash
unset TADA_CLIENT_SERVER
export TADA_GLOBAL_CONFIG='$OUT/global-config'
export TADA_GLOBAL_DATA='$OUT/global-data'
export TADA_CACHE_DIR='$OUT/cache'
export TADA_DISABLE_OPENCODE=1
cd '$PROJ'
# --yolo auto-approves tools, so contain the run: shell commands go
# through Ta-Da's macOS sandbox (writes limited to the project, temp and
# build caches; no network), and the process starts from a minimal
# environment with HOME inside the output dir so it inherits no secrets.
mkdir -p '$OUT/home'
exec env -i PATH='$PATH' TERM="\${TERM:-xterm-256color}" LANG=en_US.UTF-8 \\
	HOME='$OUT/home' GOCACHE='$OUT/gocache' GOMODCACHE='$OUT/gomodcache' GOTOOLCHAIN=local \\
	TADA_GLOBAL_CONFIG='$OUT/global-config' TADA_GLOBAL_DATA='$OUT/global-data' \\
	TADA_CACHE_DIR='$OUT/cache' TADA_DISABLE_OPENCODE=1 \\
	'$OUT/bin/tada' --yolo -c '$PROJ' -D '$DATA'
EOF
chmod +x "$OUT/run-tada.sh"
echo "project $PROJ, data $DATA"

step "Drive the TUI (${COLS}x${ROWS})"
PROMPT="This directory is a Go module named example.com/slugdemo (go.mod exists). \
Create package slug in the directory ./slug with a file slug.go exporting \
func Slugify(s string) string. Rules: lowercase the input; keep ASCII letters \
a-z and digits 0-9; every run of any other characters (spaces, punctuation, \
underscores, non-ASCII letters) becomes a single '-'; trim leading and \
trailing '-'. Also write table-driven tests in slug/slug_test.go. Then run \
go test ./... and fix anything until it passes. Keep it small."
printf '%s\n' "$PROMPT" >"$OUT/prompt.txt"

tmx new-session -d -s e2e -x "$COLS" -y "$ROWS" "$OUT/run-tada.sh"
# Wait for the editor to come up (the DB exists and the pane has drawn).
for _ in $(seq 1 60); do
	[ -f "$DB" ] && [ "$(tmx capture-pane -p -t e2e | grep -c .)" -gt 3 ] && break
	sleep 1
done
sleep 3
tmx capture-pane -p -t e2e >"$OUT/pane-start.txt"
tmx send-keys -t e2e -l "$PROMPT"
sleep 1
tmx send-keys -t e2e Enter
start=$(date +%s)

# Done when the newest message is a finished assistant reply that did not
# stop for a tool call, and it stays that way for three polls in a row.
last_sql="SELECT m.role || '|' || COALESCE((SELECT json_extract(p.value, '\$.data.reason')
	FROM json_each(m.parts) p WHERE json_extract(p.value, '\$.type') = 'finish'), '')
	FROM messages m ORDER BY m.rowid DESC LIMIT 1;"
stable=0
finished=0
while :; do
	now=$(date +%s)
	elapsed=$((now - start))
	if [ "$elapsed" -ge "$TIMEOUT" ]; then break; fi
	if ! tmx has-session -t e2e 2>/dev/null; then
		echo "Ta-Da exited early"
		break
	fi
	state=$(q "$last_sql" || true)
	case "$state" in
	assistant\|end_turn | assistant\|error | assistant\|canceled | assistant\|max_tokens | assistant\|unknown)
		stable=$((stable + 1)) ;;
	*) stable=0 ;;
	esac
	if [ "$stable" -ge 3 ]; then
		finished=1
		break
	fi
	if [ $((elapsed % 60)) -lt 5 ]; then
		echo "  ${elapsed}s: $(q 'SELECT COUNT(*) FROM messages;' || echo 0) messages, last=${state:-none}"
	fi
	# Keep live frames: the TUI uses the alternate screen, so there is no
	# scrollback to capture later.
	tmx capture-pane -p -t e2e >>"$FRAMES" 2>/dev/null || true
	echo "----- frame ${elapsed}s" >>"$FRAMES"
	sleep 5
done
tmx capture-pane -p -t e2e -S - >"$PANE" 2>/dev/null || true
tmx capture-pane -p -e -t e2e -S - >"$OUT/pane-ansi.txt" 2>/dev/null || true
# Page through the finished chat from the top so the render checks see
# every message, not just the last screen.
: >"$SCROLL"
if tmx has-session -t e2e 2>/dev/null; then
	tmx send-keys -t e2e Tab
	sleep 1
	tmx send-keys -t e2e g
	sleep 1
	prev=""
	for _ in $(seq 1 300); do
		cur=$(tmx capture-pane -p -t e2e)
		[ "$cur" = "$prev" ] && break
		printf '%s\n----- page\n' "$cur" >>"$SCROLL"
		prev=$cur
		tmx send-keys -t e2e f
		sleep 0.5
	done
fi
cp "$DB" "$OUT/tada-snapshot.db" 2>/dev/null || true

step "Checks"
if [ "$finished" = 1 ]; then
	pass "agent finished in $(($(date +%s) - start))s (last: $state)"
else
	fail "agent did not finish within ${TIMEOUT}s (last: ${state:-none})"
fi
case "$state" in
*end_turn) pass "final finish reason end_turn" ;;
*) fail "final finish reason is '${state#*|}', want end_turn" ;;
esac

# Real tool calls, stored as tool_call parts rather than text.
tool_sql="SELECT json_extract(p.value, '\$.data.name') FROM messages m, json_each(m.parts) p
	WHERE json_extract(p.value, '\$.type') = 'tool_call';"
q "$tool_sql" | sort | uniq -c | sort -rn >"$OUT/tool-calls.txt" || true
ncalls=$(awk '{s += $1} END {print s + 0}' "$OUT/tool-calls.txt")
nwrite=$(awk '$2 ~ /^(write|edit|multiedit|apply_patch)$/ {s += $1} END {print s + 0}' "$OUT/tool-calls.txt")
nbash=$(awk '$2 == "bash" {s += $1} END {print s + 0}' "$OUT/tool-calls.txt")
calls=$(awk '{printf "%s%s=%s", sep, $2, $1; sep=" "}' "$OUT/tool-calls.txt")
if [ "$nwrite" -gt 0 ] && [ "$nbash" -gt 0 ]; then
	pass "real tool calls: $ncalls ($calls)"
else
	fail "tool calls missing write/edit or bash: $ncalls (${calls:-none})"
fi

# Tool calls the model wrote as text instead of calling the tool.
text_sql="SELECT json_extract(p.value, '\$.data.text') FROM messages m, json_each(m.parts) p
	WHERE m.role = 'assistant' AND json_extract(p.value, '\$.type') = 'text';"
q "$text_sql" >"$OUT/assistant-text.txt" || true
if grep -Eq '^[[:space:]]*(<tool_call>|\{[[:space:]]*"(name|tool|function)"[[:space:]]*:)' "$OUT/assistant-text.txt"; then
	info "model wrote tool-call-shaped text (see assistant-text.txt)"
fi

nsum=$(q 'SELECT COUNT(*) FROM messages WHERE is_summary_message = 1;' || echo 0)
# Injected context (the git status snapshot) is stored as a user message;
# only count what was typed.
nturns=$(q "SELECT COUNT(*) FROM messages m WHERE m.role = 'user' AND NOT EXISTS (
	SELECT 1 FROM json_each(m.parts) p
	WHERE json_extract(p.value, '\$.data.text') LIKE '<git_status%');" || echo 0)
info "summary messages: $nsum, user turns: $nturns, messages: $(q 'SELECT COUNT(*) FROM messages;' || echo 0)"
if [ "$nturns" -le 1 ] && [ "$nsum" -gt 0 ]; then
	# Allowed only if the window genuinely filled; record it for review.
	info "auto-summarize fired during the first turn (check the log for why)"
fi

# Pane checks. Final render = the last screen plus the paged-through chat;
# live frames also catch mid-stream output, which is reported but only
# fails for the "Claude:" prefix.
cat "$PANE" "$SCROLL" >"$OUT/pane-final.txt"
FINAL=$OUT/pane-final.txt
if [ -s "$PANE" ]; then
	pass "pane captured ($(grep -c -- '----- page' "$SCROLL" || true) chat pages, $(grep -c -- '----- frame' "$FRAMES" || true) live frames)"
else
	fail "pane capture empty"
fi
# A compact JSON object with a tool-call key on one line is an unrendered
# blob; the JSON-reply fix pretty-prints so each key sits on its own line.
json_re='\{[[:space:]]*"(name|tool|tool_name|function|arguments|parameters)"[[:space:]]*:'
if grep -nE "$json_re" "$FINAL" >"$OUT/pane-json.txt"; then
	fail "raw JSON tool-call blob in pane: $(head -n 1 "$OUT/pane-json.txt" | cut -c1-100)"
else
	pass "no raw JSON tool-call blob in pane"
	if grep -nE "$json_re" "$FRAMES" >"$OUT/frames-json.txt"; then
		info "JSON tool-call blob seen mid-stream only: $(head -n 1 "$OUT/frames-json.txt" | cut -c1-100)"
	fi
fi
heading_re='^[^[:alnum:]]*#{0,3} ?(Goal|Progress|Key Decisions|Next Steps|Critical Context)[[:space:]]*$'
if [ "$(grep -cE "$heading_re" "$FINAL" || true)" -ge 2 ]; then
	grep -nE "$heading_re" "$FINAL" >"$OUT/pane-summary.txt"
	fail "summary heading wall in pane: $(tr '\n' ' ' <"$OUT/pane-summary.txt" | cut -c1-100)"
else
	pass "no summary heading wall in pane"
	if [ "$(grep -cE "$heading_re" "$FRAMES" || true)" -ge 2 ]; then
		info "summary headings seen mid-stream (expected while a summary streams)"
	fi
fi
if [ "$nsum" -gt 0 ]; then
	if grep -q 'Context summarized' "$FINAL"; then
		pass "summary shown as collapsed 'Context summarized' line"
	else
		fail "summary happened but no collapsed 'Context summarized' line in pane"
	fi
fi
if cat "$FINAL" "$FRAMES" | grep -n 'Claude:' >"$OUT/pane-claude.txt"; then
	fail "'Claude:' prefix in pane: $(head -n 1 "$OUT/pane-claude.txt" | cut -c1-100)"
else
	pass "no 'Claude:' prefix in pane"
fi

# Auto-lean.
if [ -f "$LOG" ] && grep -q 'Using the lean tool profile' "$LOG"; then
	info "auto-lean fired: $(grep -m1 'Using the lean tool profile' "$LOG" | cut -c1-200)"
else
	info "auto-lean did not fire (prompt+tools under half of the context window)"
fi
if [ -f "$LOG" ]; then
	nerr=$(grep -c '"level":"ERROR"' "$LOG" || true)
	info "log errors: $nerr"
fi

# Independent verification of what Ta-Da built.
if [ -f "$PROJ/slug/slug.go" ]; then
	pass "slug/slug.go exists"
else
	fail "slug/slug.go missing (files: $(cd "$PROJ" && find . -name '*.go' -not -path './.git/*' | tr '\n' ' '))"
fi
own_tests=$(find "$PROJ/slug" -name '*_test.go' 2>/dev/null | wc -l | tr -d ' ')
if [ "$own_tests" -gt 0 ]; then
	pass "Ta-Da wrote $own_tests test file(s)"
else
	fail "Ta-Da wrote no test files"
fi
if (cd "$PROJ" && go test ./... >"$OUT/go-test-tada.txt" 2>&1); then
	pass "Ta-Da's own tests pass (go test ./...)"
else
	fail "Ta-Da's own tests fail: $(tail -n 3 "$OUT/go-test-tada.txt" | tr '\n' ' ' | cut -c1-160)"
fi

mkdir -p "$PROJ/slug"
cat >"$PROJ/slug/zz_e2e_hidden_test.go" <<'EOF'
package slug_test

import (
	"testing"

	"example.com/slugdemo/slug"
)

// Written by scripts/e2e-local.sh, not by the agent.
func TestE2EHiddenSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello World":                   "hello-world",
		"  Hello,   World!  ":           "hello-world",
		"Go_is--fun":                    "go-is-fun",
		"ABC123":                        "abc123",
		"":                              "",
		"---":                           "",
		"!!!":                           "",
		"a.b.c":                         "a-b-c",
		"café au lait":                  "caf-au-lait",
		"Tabs\tand\nnewlines":           "tabs-and-newlines",
		"x":                             "x",
		"-Leading and trailing-":        "leading-and-trailing",
		"100% Pure (Go) 2.0":            "100-pure-go-2-0",
		"ÜBER cool":                     "ber-cool",
		"already-a-slug":                "already-a-slug",
		"MiXeD CaSe 42 Times":           "mixed-case-42-times",
		"multiple     spaces    inside": "multiple-spaces-inside",
	}
	for in, want := range cases {
		if got := slug.Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
EOF
cp "$PROJ/slug/zz_e2e_hidden_test.go" "$OUT/"
if (cd "$PROJ" && go test -count=1 -run TestE2EHiddenSlugify ./slug >"$OUT/go-test-hidden.txt" 2>&1); then
	pass "hidden spec tests pass"
else
	fail "hidden spec tests fail: $(grep -m3 -E 'want|FAIL|error' "$OUT/go-test-hidden.txt" | tr '\n' ' ' | cut -c1-200)"
fi
if (cd "$PROJ" && go vet ./... >"$OUT/go-vet.txt" 2>&1); then
	pass "go vet clean"
else
	fail "go vet: $(head -n 2 "$OUT/go-vet.txt" | tr '\n' ' ' | cut -c1-160)"
fi

summary
