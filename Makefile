#!/bin/sh

PROGNAME=richmail

# build targets
$(PROGNAME): *.go
	@env GOPATH=/tmp/go go get -d && env GOPATH=/tmp/go CGO_ENABLED=0 GOARCH=${_ARCH} GOOS=${_OS} go build -trimpath -o $(PROGNAME)
	@-strip $(PROGNAME) 2>/dev/null || true
	@-upx -9 $(PROGNAME) 2>/dev/null || true
clean:
distclean:
	@rm -f $(PROGNAME)

# run targets
run: $(PROGNAME)
	@-./$(PROGNAME) template.html month=$$(date -d '1 month ago' +%b). year=$$(date -d '1 month ago' +%Y)
