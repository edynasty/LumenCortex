git ls-files "*.go" | grep -v "^bench/evidence/" | xargs gofmt -l
