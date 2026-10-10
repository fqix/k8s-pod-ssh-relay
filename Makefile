NS        := k8s-pod-ssh-relay
ARCH      ?= $(shell go env GOARCH)
# Must match the Ingress host in deploy/k8s.yaml. It doesn't need to resolve:
# relay-client -connect-to sends the TCP connection to the minikube IP.
RELAY_HOST := k8s-pod-ssh-relay.test
INGRESS   ?= $(shell minikube ip):443

.PHONY: all build image deploy ssh-config ssh pods logs clean

all: deploy ssh-config

build:
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 go build -o bin/linux/ ./cmd/relay-server
	go build -o bin/ ./cmd/relay-client

image: build
	minikube image build -t relay-server:latest -f Dockerfile.server .
	minikube image build -t relay-target:latest -f Dockerfile.target .

keys/id_ed25519:
	mkdir -p keys
	ssh-keygen -t ed25519 -N '' -C k8s-pod-ssh-relay -f keys/id_ed25519

# Self-signed CA and an ingress certificate for $(RELAY_HOST).
keys/ca.crt:
	mkdir -p keys
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
	  -subj '/CN=k8s-pod-ssh-relay CA' -addext 'basicConstraints=critical,CA:TRUE' \
	  -keyout keys/ca.key -out keys/ca.crt

keys/tls.crt: keys/ca.crt
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
	  -subj '/CN=$(RELAY_HOST)' -keyout keys/tls.key -out keys/tls.csr
	printf 'subjectAltName=DNS:$(RELAY_HOST)\nextendedKeyUsage=serverAuth\n' > keys/tls.ext
	openssl x509 -req -in keys/tls.csr -CA keys/ca.crt -CAkey keys/ca.key -CAcreateserial \
	  -days 365 -extfile keys/tls.ext -out keys/tls.crt

deploy: image keys/id_ed25519 keys/tls.crt
	kubectl create ns $(NS) --dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(NS) create secret tls relay-tls --cert=keys/tls.crt --key=keys/tls.key --dry-run=client -o yaml | kubectl apply -f -
	sed "s|__AUTHORIZED_KEYS__|$$(cat keys/id_ed25519.pub)|" deploy/k8s.yaml | kubectl apply -f -
	kubectl -n $(NS) rollout restart deploy/relay-server deploy/target
	kubectl -n $(NS) rollout status deploy/relay-server deploy/target

# Generates an ssh config so that `ssh -F ssh_config <pod-name>.relay` works.
ssh-config: keys/id_ed25519 keys/ca.crt
	@printf '%s\n' \
	  'Host *.relay' \
	  '  User demo' \
	  '  IdentityFile $(CURDIR)/keys/id_ed25519' \
	  '  IdentitiesOnly yes' \
	  '  StrictHostKeyChecking no' \
	  '  UserKnownHostsFile /dev/null' \
	  '  LogLevel ERROR' \
	  '  SetEnv TERM=xterm-256color' \
	  '  ProxyCommand $(CURDIR)/bin/relay-client -relay wss://$(RELAY_HOST) -ca $(CURDIR)/keys/ca.crt -connect-to $(INGRESS) -i $(CURDIR)/keys/id_ed25519 -target %h' \
	  > ssh_config
	@echo "wrote ssh_config; try: ssh -F ssh_config \$$(kubectl -n $(NS) get pod -l app=target -o name | head -1 | cut -d/ -f2).relay"

pods:
	@kubectl -n $(NS) get pods -o custom-columns='NAME:.metadata.name,SSH:.metadata.annotations.k8s-pod-ssh-relay/ssh,PORT:.metadata.annotations.k8s-pod-ssh-relay/ssh-port,STATUS:.status.phase,IP:.status.podIP,KEYS:.metadata.annotations.k8s-pod-ssh-relay/authorized-keys'

ssh:
	ssh -F ssh_config $$(kubectl -n $(NS) get pod -l app=target -o jsonpath='{.items[0].metadata.name}').relay

logs:
	kubectl -n $(NS) logs deploy/relay-server -f

clean:
	kubectl delete ns $(NS) --ignore-not-found
	rm -rf bin ssh_config
