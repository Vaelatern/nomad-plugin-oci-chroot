BINARY=oci-chroot
PLUGIN_DIR=nomad-plugin-dev

.PHONY: build install clean

build:
	go build -o $(BINARY) .

install: build
	mkdir -p $(PLUGIN_DIR)
	cp $(BINARY) $(PLUGIN_DIR)/$(BINARY)

clean:
	rm -f $(BINARY) nomad-plugin-dev/*

run-dev: build
	sudo ./$(BINARY)
