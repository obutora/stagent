package install

import "github.com/obutora/stagent/internal/paths"

func currentSID() string {
	sid, _ := paths.CurrentUserSID()
	return sid
}
