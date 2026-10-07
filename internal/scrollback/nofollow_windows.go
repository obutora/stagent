package scrollback

// oNoFollow is 0 on Windows: os.OpenFile has no O_NOFOLLOW there, and the
// data directory is under the user's profile.
const oNoFollow = 0
