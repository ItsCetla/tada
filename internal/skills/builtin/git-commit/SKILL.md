---
name: git-commit
description: Use when the user asks for a git commit, a new or amended commit message, or a pull request title and body.
---

# Git commits and pull requests

Only commit or open a PR when the user asks. Never update git config, never
push unless asked (a PR needs a push), and never use interactive (`-i`)
flags. If the bash tool description lists attribution lines for commits or
PRs, end every commit message or PR body with them exactly.

## Message quality

These rules apply to commit messages, PR titles and PR bodies:

- Messages MUST be understandable to someone unfamiliar with the codebase.
- Litmus test: a new contributor reading only the commit message or PR
  title/body should understand what problem this solves, why it matters,
  and the impact without opening files, reading the diff, or knowing
  internal code names.
- Avoid code identifiers, filenames, function names, and implementation
  details unless they are necessary for understanding the user-facing
  impact.
- Bad: "Add NameFromHex with sync.Once lazy init"
- Good: "Improve color name lookup performance while keeping startup fast"

## Commit messages

Commit messages are for future readers scanning history.

- Draft a concise 1-2 sentence message focusing on why the change exists
  and what outcome it enables, not a list of files or implementation
  details.
- Use clear, accurate verbs ("add" = new capability, "update" =
  enhancement, "fix" = bug fix) and avoid generic messages.
- The first line MUST be under 72 characters.
- Add a body only when it is needed to explain the reasoning, tradeoffs,
  or important context; wrap body lines at 72 characters.
- If the change is internal-only, still describe the benefit or
  maintenance outcome rather than naming private code.
- Bad: "fix: nil pointer in session.go"
- Good: "fix: prevent session loading from crashing on missing metadata"
- Bad: "refactor: move PromptBuilder into internal/agent"
- Good: "refactor: make prompt assembly easier to maintain"

## Creating a commit

1. In a single message, run these in parallel (IMPORTANT for speed):
   - `git status` (untracked files)
   - `git diff` (staged and unstaged changes)
   - `git log` (recent commit message style)
2. Add relevant untracked files to staging. Don't commit files that were
   already modified at conversation start unless they are relevant. Don't
   stage unrelated files.
3. Analyze the staged changes in `<commit_analysis>` tags:
   - List changed/added files and summarize the nature of the change
     (feature, enhancement, bug fix, refactoring, test, docs).
   - Brainstorm the purpose, assess the project impact, and check for
     sensitive information.
   - Don't use tools beyond git here.
4. Draft the message following the rules above and check it against the
   litmus test.
5. Commit with a HEREDOC, adding the attribution lines (if any) after a
   blank line:

   ```sh
   git commit -m "$(cat <<'EOF'
   Commit message here.

   <attribution lines, if any>
   EOF
   )"
   ```

6. If a pre-commit hook fails, retry ONCE. If it fails again, the hook is
   preventing the commit. If it succeeds but modified files, you MUST
   amend.
7. Run `git status` to verify.

Notes: use `git commit -am` when possible, no empty commits, when rebasing
always use `-m`, and return an empty response afterwards.

## Pull requests

Use the `gh` command for all GitHub tasks when it is available.

1. In a single message, run these in parallel (VERY IMPORTANT for speed):
   - `git status` (untracked files)
   - `git diff` (staged and unstaged changes)
   - check whether the branch tracks a remote and is up to date
   - `git log` and `git diff main...HEAD` (full history since diverging
     from main)
2. Create a new branch if needed.
3. Commit changes if needed (follow "Creating a commit").
4. Push to the remote with `-u` if needed.
5. Analyze the changes in `<pr_analysis>` tags:
   - List the commits since diverging from main and summarize the nature
     of the changes.
   - Brainstorm the purpose, assess the project impact, and check for
     sensitive information.
   - Don't use tools beyond git here.
6. Draft the PR message following the rules above:
   - A concise (1-2 bullet points) summary focusing on "why" that reflects
     ALL changes since main, not just the latest commit.
   - Avoid generic summaries; check it against the litmus test.
7. Create the PR with a HEREDOC, adding the PR attribution line (if any)
   at the end:

   ```sh
   gh pr create --title "title" --body "$(cat <<'EOF'
   <summary>

   <attribution line, if any>
   EOF
   )"
   ```

Return an empty response afterwards; the user sees the `gh` output.
