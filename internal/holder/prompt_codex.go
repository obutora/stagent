package holder

// codexMenus: Codex highlights an option with › and indents every line of
// its dialog: the edge is the nearest line above the options that starts in
// the first column (the transcript entry above, e.g. `• Running …`).
var codexMenus = menuStyle{
	cursor:     "›",
	dialogEdge: startsInFirstColumn,
}
