package internal

// Version is parsed by the packaging pipeline in ~/git/conf/packages/Makefile
// with `grep -m1 '^const Version' internal/version.go | sed 's/.*"\(.*\)"/\1/' | tr -d v`,
// so the constant must stay a plain double-quoted literal on one line that
// starts with `const Version`.
//
// NetBSD package names use a dash to separate name from version, so this must
// never contain a dash (use "1.2.3rc1", not "1.2.3-rc1").
const Version = "v0.11.4"
