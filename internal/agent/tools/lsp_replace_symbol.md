Replace, insert next to, or delete a whole symbol (function, method, class, struct, type) by name. Boundaries come from LSP document symbols, so whitespace can't break the match. Prefer this over `edit` whenever the change covers an entire symbol; use `edit` for partial or within-line changes and for files without an LSP.

Actions:
- `replace` (default): replace the entire symbol, signature and body
- `add_before`: insert text before the symbol
- `add_after`: insert text after the symbol
- `delete`: remove the symbol

Returns diagnostics after the edit; fix any you introduced.
