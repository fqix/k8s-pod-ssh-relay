NS        := k8s-pod-ssh-relay
ARCH      ?= $(shell go env GOARCH)
# The Ingress host. Locally it doesn't need to resolve: relay-client
# -connect-to sends the TCP connection to the minikube IP over plain HTTP.
# In production, deploy with your domain and terminate TLS for it at the
# Ingress, then use RELAY_URL=wss://<domain> and an empty INGRESS:
#   make deploy RELAY_HOST=relay.example.com
#   make ssh-config RELAY_URL=wss://relay.example.com INGRESS=
RELAY_HOST ?= k8s-pod-ssh-relay.test
RELAY_URL  ?= ws://$(RELAY_HOST)
INGRESS    ?= $(shell minikube ip):80
CONNECT_TO := $(if $(INGRESS),-connect-to $(INGRESS))

.PHONY: all build test lint image deploy ssh-config ssh pods logs clean vscode vscode-settings

all: deploy ssh-config

build:
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 go build -o bin/linux/ ./cmd/relay-server
	go build -o bin/ ./cmd/relay-client

test:
	go test -race ./...
	cd vscode && npm run -s typecheck

# Needs shellcheck (brew install shellcheck); the scripts run in pods as POSIX sh.
lint:
	go vet ./...
	shellcheck -s sh internal/vscodeserver/scripts/*.sh

image: build
	minikube image build -t relay-server:latest -f Dockerfile.server .
	minikube image build -t relay-target:latest -f Dockerfile.target .

keys/id_ed25519:
	mkdir -p keys
	ssh-keygen -t ed25519 -N '' -C k8s-pod-ssh-relay -f keys/id_ed25519

keys/session.key:
	mkdir -p keys
	openssl rand -hex 32 | tr -d '\n' > keys/session.key

deploy: image keys/id_ed25519 keys/session.key
	kubectl create ns $(NS) --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(NS) create secret generic relay-session-key --from-file=key=keys/session.key --dry-run=client -o yaml | kubectl apply -f -
	sed -e "s|__AUTHORIZED_KEYS__|$$(cat keys/id_ed25519.pub)|" -e "s|__RELAY_HOST__|$(RELAY_HOST)|" deploy/k8s.yaml | kubectl apply -f -
	kubectl -n $(NS) rollout restart deploy/relay-server deploy/target
	kubectl -n $(NS) rollout status deploy/relay-server deploy/target

# Generates an ssh config so that `ssh -F ssh_config <pod>.<namespace>.relay` works.
ssh-config: keys/id_ed25519
	@printf '%s\n' \
	  'Host *.relay' \
	  '  User demo' \
	  '  IdentityFile $(CURDIR)/keys/id_ed25519' \
	  '  IdentitiesOnly yes' \
	  '  StrictHostKeyChecking no' \
	  '  UserKnownHostsFile /dev/null' \
	  '  LogLevel ERROR' \
	  '  SetEnv TERM=xterm-256color' \
	  '  ProxyCommand $(CURDIR)/bin/relay-client -relay $(RELAY_URL) $(CONNECT_TO) -i $(CURDIR)/keys/id_ed25519 -target %h' \
	  > ssh_config
	@echo "wrote ssh_config; try: ssh -F ssh_config \$$(kubectl -n $(NS) get pod -l app=target -o name | head -1 | cut -d/ -f2).$(NS).relay"

# Builds the Remote - PodRelay VS Code extension (vscode/remote-podrelay-*.vsix).
vscode:
	cd vscode && npm ci && npm run package

# Prints VS Code user settings for this demo cluster.
vscode-settings: keys/id_ed25519
	@printf '%s\n' \
	  '"k8sPodSshRelay.relayUrl": "$(RELAY_URL)",' \
	  '"k8sPodSshRelay.connectTo": "$(INGRESS)",' \
	  '"k8sPodSshRelay.identityFile": "$(CURDIR)/keys/id_ed25519",' \
	  '"k8sPodSshRelay.user": "demo"'

pods:
	@kubectl get pods -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,SSH:.metadata.annotations.k8s-pod-ssh-relay/ssh,PORT:.metadata.annotations.k8s-pod-ssh-relay/ssh-port,STATUS:.status.phase,IP:.status.podIP,KEYS:.metadata.annotations.k8s-pod-ssh-relay/authorized-keys'

ssh:
	ssh -F ssh_config $$(kubectl -n $(NS) get pod -l app=target -o jsonpath='{.items[0].metadata.name}').$(NS).relay

logs:
	kubectl -n $(NS) logs deploy/relay-server -f

clean:
	kubectl delete ns $(NS) --ignore-not-found
	rm -rf bin ssh_config
