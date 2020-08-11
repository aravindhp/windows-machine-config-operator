# dist-git Dockerfile
# We cannot use a golang image as building the kubelet requires rsync and that is not present plus there is no easy way
# to iustall it.
FROM registry.access.redhat.com/ubi8/ubi-minimal as build
LABEL stage=build
#RUN microdnf -y update
RUN microdnf -y install rsync make go git tar findutils diffutils


## Build WMCO
WORKDIR /build/wmco
COPY . .
RUN make build

# Build WMCB
WORKDIR /build/wmcb
RUN tar -zxf /build/wmco/wmcb-4.6-dcb438a.tgz
RUN make build

# Build hybrid-overlay
WORKDIR /build/ovn-kubernetes
RUN tar -zxf /build/wmco/ovn-kubernetes-4.6-df7b893d.tgz
WORKDIR /build/ovn-kubernetes/go-controller/
RUN make windows

# Build CNI plugins
WORKDIR /build/containernetworking-plugins
RUN tar -zxf /build/wmco/containernetworking-plugins-4.6-03ee7f3.tgz
ENV CGO_ENABLED=0
RUN ./build_windows.sh

# Build kubelet and kube-proxy
WORKDIR /build/kubernetes
RUN tar -zxf /build/wmco/kubernetes-4.6-5241b27b8ac.tgz
RUN KUBE_BUILD_PLATFORMS=windows/amd64 make WHAT=cmd/kubelet
RUN KUBE_BUILD_PLATFORMS=windows/amd64 make WHAT=cmd/kube-proxy

# Build the operator image with following payload structure
# /payload/
#├── cni
#│   ├── flannel.exe
#│   ├── host-local.exe
#│   ├── win-bridge.exe
#│   ├── win-overlay.exe
#│   └── cni-conf-template.json
#├── hybrid-overlay-node.exe
#├── kube-node
#│   ├── kubelet.exe
#│   └── kube-proxy.exe
#├── powershell
#│   └── wget-ignore-cert.ps1
#│   └── hns.psm1
#└── wmcb.exe

FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
LABEL stage=operator

# Copy wmcb.exe
WORKDIR /payload/
COPY --from=build /build/wmcb/wmcb.exe .

# Copy hybrid-overlay-node.exe
COPY --from=build /build/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe .

# Copy kubelet.exe and kube-proxy.exe
WORKDIR /payload/kube-node/
COPY --from=build /build/kubernetes/_output/local/bin/windows/amd64/kubelet.exe .
COPY --from=build /build/kubernetes/_output/local/bin/windows/amd64/kube-proxy.exe .

# Copy CNI plugin binaries and CNI config template cni-conf-template.json
RUN mkdir /payload/cni/
WORKDIR /payload/cni/
COPY --from=build /build/containernetworking-plugins/bin/flannel.exe .
COPY --from=build /build/containernetworking-plugins/bin/host-local.exe .
COPY --from=build /build/containernetworking-plugins/bin/win-bridge.exe .
COPY --from=build /build/containernetworking-plugins/bin/win-overlay.exe .
COPY --from=build /build/wmco/pkg/internal/cni-conf-template.json .

# Copy required powershell scripts
RUN mkdir /payload/powershell/
WORKDIR /payload/powershell/
COPY --from=build /build/wmco/pkg/internal/wget-ignore-cert.ps1 .
COPY --from=build /build/wmco/pkg/internal/hns.psm1 .

WORKDIR /

ENV OPERATOR=/usr/local/bin/wmco \
    USER_UID=1001 \
    USER_NAME=wmco

# install operator binary
COPY --from=build /build/wmco/build/_output/bin/windows-machine-config-operator ${OPERATOR}

COPY --from=build /build/wmco/build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}
