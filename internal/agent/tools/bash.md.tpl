Run a command in a fresh mvdan/sh shell (Bash-compatible; forward slashes, absolute paths, no `cd`). Always set `description`.
- Banned (error; tell the user): curl, wget, ssh, package managers, sudo, system admin. HTTP: use the fetch tool.
- Use the grep/glob/view/ls tools, not find/grep/cat/head/tail/ls.
{{- if .RgAvailable }} In pipelines prefer `rg`.{{- end }}
- Output over {{ .MaxOutputLength }} chars is cut to its first and last half plus a file with all of it: grep or view that, don't rerun.
- After `auto_background_after` s (default {{ .DefaultAutoBackgroundAfter }}) a command becomes a background job.
- Servers/watchers: run_in_background=true, never a trailing `&`; job_output reads, job_kill stops. Builds, tests, git: foreground.

<git>
- Commit, push or PR only when asked; no git config edits, -i flags or empty commits.
- First load the `git-commit` skill.
{{- if or .Attribution.GeneratedWith (eq .Attribution.TrailerStyle "assisted-by") (eq .Attribution.TrailerStyle "co-authored-by") }}
- End commit messages, after a blank line, with:
{{- if .Attribution.GeneratedWith }}
  💘 Generated with Ta-Da
{{- end }}
{{- if eq .Attribution.TrailerStyle "assisted-by" }}
  Assisted-by: Ta-Da:{{ .ModelID }}
{{- else if eq .Attribution.TrailerStyle "co-authored-by" }}
  Co-Authored-By: Ta-Da <tada@cetla.dev>
{{- end }}
{{- end }}
{{- if .Attribution.GeneratedWith }}
- End PR bodies with: 💘 Generated with Ta-Da
{{- end }}
{{- if .GhAvailable }}
- Use `gh` for GitHub.
{{- end }}
</git>
