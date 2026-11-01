package release

import "regexp"

var versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
