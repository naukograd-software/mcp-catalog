.PHONY: build install install-user uninstall run clean

BINARY=mcp-manager
BUILD_DIR=./build

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/mcp-manager

run: build
	$(BUILD_DIR)/$(BINARY) --port 9847

# Install system-wide (requires sudo)
install: build
	sudo cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/
	sudo cp mcp-manager@.service /etc/systemd/system/
	@echo ""
	@echo "Installed! Enable with:"
	@echo "  sudo systemctl enable --now mcp-manager@$$USER"
	@echo ""
	@echo "UI available at: http://localhost:9847"

# Install for current user (no sudo)
install-user: build
	mkdir -p ~/.local/bin
	cp $(BUILD_DIR)/$(BINARY) ~/.local/bin/
	mkdir -p ~/.config/systemd/user
	cp mcp-manager.user.service ~/.config/systemd/user/mcp-manager.service
	systemctl --user daemon-reload
	@echo ""
	@echo "Installed! Enable with:"
	@echo "  systemctl --user enable --now mcp-manager"
	@echo ""
	@echo "UI available at: http://localhost:9847"

uninstall:
	-sudo systemctl stop mcp-manager@$$USER
	-sudo systemctl disable mcp-manager@$$USER
	-systemctl --user stop mcp-manager
	-systemctl --user disable mcp-manager
	-sudo rm -f /usr/local/bin/$(BINARY)
	-sudo rm -f /etc/systemd/system/mcp-manager@.service
	-rm -f ~/.local/bin/$(BINARY)
	-rm -f ~/.config/systemd/user/mcp-manager.service

clean:
	rm -rf $(BUILD_DIR)
