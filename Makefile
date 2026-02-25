.PHONY: build install uninstall run clean

BINARY=mcp-manager
BUILD_DIR=./build

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/mcp-manager

run: build
	$(BUILD_DIR)/$(BINARY) --port 9847

# Install or reinstall system-wide (requires sudo)
install: build
	sudo install -m 0755 $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	sudo install -m 0644 mcp-manager.service /etc/systemd/system/mcp-manager.service
	sudo systemctl daemon-reload
	@if sudo systemctl is-active --quiet mcp-manager; then \
		sudo systemctl restart mcp-manager; \
		echo "Restarted mcp-manager"; \
	fi
	@echo ""
	@echo "Installed/Reinstalled! Enable with:"
	@echo "  sudo systemctl enable --now mcp-manager"
	@echo ""
	@echo "UI available at: http://localhost:9847"

uninstall:
	-sudo systemctl stop mcp-manager
	-sudo systemctl disable mcp-manager
	-sudo rm -f /usr/local/bin/$(BINARY)
	-sudo rm -f /etc/systemd/system/mcp-manager.service
	-sudo rm -f /etc/systemd/system/mcp-manager@.service

clean:
	rm -rf $(BUILD_DIR)
