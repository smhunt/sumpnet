// web/ holds no Go code. This go.mod makes it a separate module so `go ./...`
// and golangci-lint in the repository root never walk node_modules (some npm
// packages, e.g. flatted, ship .go files).
module github.com/smhunt/sumpnet/web

go 1.26
