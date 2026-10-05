# GitHub repository
REPO := ssp-data/neomd
INSTALL_PATH := ~/.local/bin/neomd
LOG := ~/.local/share/neomd/daemon.log

.DEFAULT_GOAL := run-headless

## run-headless: start neomd daemon in background
run-headless:
	nohup neomd --headless >> $(LOG) 2>&1 &

## run-syncthing: start syncthing in background (access UI via SSH tunnel from laptop: ssh -L 8385:localhost:8384 ti)
run-syncthing:
	nohup syncthing >> ~/syncthing.log 2>&1 &

## restart: stop and restart daemon
restart: stop run-headless

## stop: stop daemon
stop:
	pkill neomd || true
	sleep 1

## status: is the daemon running, and did its last screening cycle succeed?
status:
	@ps aux | grep '[n]eomd --headless' || echo "ERROR: neomd is not running"
	@echo ""
	@test -f $(LOG) || { echo "ERROR: no log at $(LOG) (renamed/deleted while running? -> make restart)"; exit 1; }
	@echo "--- last log lines ---"
	@tail -3 $(LOG)
	@echo ""
	@last=$$(tail -200 $(LOG) | grep -E 'level=ERROR|screening complete|no emails need screening' | tail -1); \
	case "$$last" in \
	*level=ERROR*) \
		echo "ERROR: last screening cycle FAILED:"; \
		echo "$$last" | sed -E 's/.*error="([^"]*)".*/  \1/'; \
		since=$$(awk '/level=ERROR/ { if (since == "") since = substr($$1, 6) } /screening complete|no emails need screening/ { since = "" } END { print since }' $(LOG)); \
		echo "  failing since: $$since (every cycle; nothing has been screened since then)"; \
		exit 1 ;; \
	"") echo "WARN: no screening result in the log yet" ;; \
	*) echo "OK: last screening cycle succeeded" ;; \
	esac

## log: follow daemon logs
log:
	tail -f $(LOG)

## version: show installed version
version:
	neomd --version

## reinstall: download latest GitHub release and restart daemon (requires v0.6.1+)
reinstall: stop
	@echo "Fetching latest release from GitHub..."
	@latest=$$(curl -s https://api.github.com/repos/$(REPO)/releases/latest | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/'); \
	version=$${latest#v}; \
	echo "Latest version: $$latest"; \
	echo "Downloading neomd_$${version}_freebsd_amd64.tar.gz..."; \
	curl -L -o /tmp/neomd.tar.gz "https://github.com/$(REPO)/releases/download/$$latest/neomd_$${version}_freebsd_amd64.tar.gz"; \
	echo "Extracting..."; \
	tar -xzf /tmp/neomd.tar.gz -C /tmp; \
	echo "Installing to $(INSTALL_PATH)..."; \
	mkdir -p ~/.local/bin; \
	mv /tmp/neomd $(INSTALL_PATH); \
	chmod +x $(INSTALL_PATH); \
	rm -f /tmp/neomd.tar.gz; \
	echo "Done! Installed $$latest"
	@$(MAKE) run-headless
	@sleep 1
	@$(MAKE) status
