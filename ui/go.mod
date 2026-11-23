// Sentinel module (adapted from C360Studio/semboids ui/go.mod, MIT): excludes
// ui/ and stray .go files inside ui/node_modules (such as flatted's Go port)
// from the root module's ./... package set.
module github.com/boxbotix/bbops/ui-exclude

go 1.26.11
