module github.com/billstark001/latexmk/packages/cli

go 1.27.0

replace github.com/billstark001/latexmk/packages/shared => ../shared

require (
	github.com/billstark001/latexmk/packages/shared v0.0.0
	github.com/bmatcuk/doublestar/v4 v4.10.0
	github.com/fsnotify/fsnotify v1.9.0
	github.com/joho/godotenv v1.5.1
)

require golang.org/x/sys v0.13.0 // indirect
