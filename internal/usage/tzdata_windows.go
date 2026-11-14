package usage

// Windows has no zoneinfo files for time.LoadLocation; usage.get sums
// days in the IANA time zone the app names.
import _ "time/tzdata"
