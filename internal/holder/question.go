package holder

// Question watch (holder.question_watch, since 0.20.0): while a question an
// agent asked through its question tool is in force, the daemon has the
// holder look for the question's UI on the screen and report once the UI
// it saw is gone (holder.question_gone): the question was answered or
// cancelled there — an Esc sends no hook. It runs on promptWatch, with
// the question UI as its menu; every screen of a question shares one
// signature, so moving through it (the highlight, claude's tabs and review
// page, omp's note box) is not the question leaving.

// questionMenu reports whether lines show a question UI of harnessID, as
// promptWatch's menu: its signature is the same for every question screen.
func questionMenu(lines []string, harnessID string) (sig string, ok bool) {
	return "", questionShown(lines, harnessID)
}

// questionShown reports whether lines (a screen, top to bottom) show the
// UI of a question harnessID asked: its menuStyle's question, or else any
// menu as it draws its menus (claude's and Codex's numbered options).
func questionShown(lines []string, harnessID string) bool {
	if shown := menuStyles[harnessID].question; shown != nil {
		return shown(lines)
	}
	_, ok := findMenu(lines, harnessID)
	return ok
}
