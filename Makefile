BINARY=oci-chroot
PLUGIN_DIR=nomad-plugin-dev

.PHONY: build install clean

build: $(BINARY)

$(BINARY):
	go build -o $(BINARY) .

install: build
	sudo rm -f /opt/nomad/plugins/$(BINARY)
	sudo cp $(BINARY) /opt/nomad/plugins/$(BINARY)
	sudo sv restart nomad

clean:
	rm -f $(BINARY) nomad-plugin-dev/*

run-dev: build
	sudo ./$(BINARY)
