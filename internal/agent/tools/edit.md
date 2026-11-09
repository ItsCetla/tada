Exact find-and-replace in a file.
- View it first; copy old_string exactly (whitespace, blank lines), with 3-5 lines of context so it matches once.
- An indentation-only mismatch is still applied and re-indented; the response says so, check it.
- On "not found", view again and copy the exact text; never guess.
- Don't re-read after success.
- Several changes to one file: multiedit. Rewrites: write. Prefer `lsp_replace_symbol`/`lsp_rename` when available.
