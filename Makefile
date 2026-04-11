.PHONY: build clean setup demo

BINARY := docksmith

build:
	go build -o $(BINARY) .

clean:
	rm -f $(BINARY)
	rm -rf ~/.docksmith

setup: build
	./setup/import-base-images.sh

demo: setup
	@echo ""
	@echo "=== Demo Step 1: Cold build (all CACHE MISS) ==="
	./$(BINARY) build -t myapp:latest ./sample-app
	@echo ""
	@echo "=== Demo Step 2: Warm build (all CACHE HIT) ==="
	./$(BINARY) build -t myapp:latest ./sample-app
	@echo ""
	@echo "=== Demo Step 3: Edit source, partial cache invalidation ==="
	@cp sample-app/config/settings.txt sample-app/config/settings.txt.bak
	@echo "# changed" >> sample-app/config/settings.txt
	./$(BINARY) build -t myapp:latest ./sample-app
	@mv sample-app/config/settings.txt.bak sample-app/config/settings.txt
	@echo ""
	@echo "=== Demo Step 4: List images ==="
	./$(BINARY) images
	@echo ""
	@echo "=== Demo Step 5: Run container ==="
	./$(BINARY) run myapp:latest
	@echo ""
	@echo "=== Demo Step 6: Run with env override ==="
	./$(BINARY) run -e GREETING=Howdy myapp:latest
	@echo ""
	@echo "=== Demo Step 7: Isolation test (PASS/FAIL) ==="
	./$(BINARY) run myapp:latest /bin/sh -c 'echo SECRET > /tmp/escape-test.txt'
	@if [ -f /tmp/escape-test.txt ]; then echo "FAIL: file escaped container!"; exit 1; else echo "PASS: file is NOT visible on host"; fi
	@echo ""
	@echo "=== Demo Step 8: Reproducibility (--no-cache rebuild) ==="
	./$(BINARY) build --no-cache -t myapp:latest ./sample-app
	@echo ""
	@echo "=== Demo Step 9: No CMD error test ==="
	@mkdir -p /tmp/docksmith-nocmd-test
	@printf 'FROM busybox:latest\nRUN echo hello > /tmp/hi.txt\n' > /tmp/docksmith-nocmd-test/Docksmithfile
	./$(BINARY) build -t nocmd:test /tmp/docksmith-nocmd-test
	@if ./$(BINARY) run nocmd:test 2>/dev/null; then echo "FAIL: should have errored with no CMD"; exit 1; else echo "PASS: correctly failed with no CMD"; fi
	@rm -rf /tmp/docksmith-nocmd-test
	@echo ""
	@echo "=== Demo Step 10: Remove image (rmi) ==="
	./$(BINARY) rmi myapp:latest
	./$(BINARY) rmi nocmd:test
	./$(BINARY) images
	@echo ""
	@echo "=== ALL DEMO STEPS PASSED ==="
