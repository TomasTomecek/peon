Name:           peon
Version:        0.1.0
Release:        1%{?dist}
Summary:        Run human-approved commands from Forgejo issues

License:        MIT
URL:            https://github.com/TomasTomecek/peon
# Tarball with vendored Go modules, produced by packaging/create-archive.sh
# (Packit runs it for every build; for releases, attach its output).
Source0:        %{name}-%{version}.tar.gz

# Go does not support every architecture Fedora builds for. The fallback
# keeps the header valid when go-srpm-macros is missing at SRPM build time;
# otherwise the literal "%%{golang_arches}" matches no architecture and
# Copr skips every chroot without writing a build log.
ExclusiveArch:  %{?golang_arches}%{!?golang_arches:x86_64 aarch64 ppc64le s390x riscv64}

BuildRequires:  golang >= 1.22
BuildRequires:  systemd-rpm-macros
# peon.service drops all capabilities. systemd 257+ authorizes managing
# units by the caller's UID 0 alone; older releases check CAP_SYS_ADMIN.
Requires:       systemd >= 257
%{?systemd_requires}

%description
peon is a per-host daemon that watches Forgejo issues for commands proposed
by agents, waits for an explicit approval reaction from the operator, runs
the approved command as root in a transient systemd unit, and records every
step in Loki.

%prep
%autosetup -p1

%build
export CGO_ENABLED=0
# No -trimpath: it strips the build directory from the debug info's source
# paths, so find-debuginfo collects no sources and the build fails with an
# empty debugsourcefiles.list.
export GOFLAGS="-mod=vendor"
go build -buildmode=pie \
    -ldflags "-B gobuildid -X main.version=%{version}-%{release}" \
    -o %{name} ./cmd/peon

%install
install -Dpm0755 %{name} %{buildroot}%{_bindir}/%{name}
install -Dpm0644 dist/peon.service %{buildroot}%{_unitdir}/peon.service
install -Dpm0600 dist/config.yaml %{buildroot}%{_sysconfdir}/peon/config.yaml
install -dm0700 %{buildroot}%{_sharedstatedir}/peon

%post
%systemd_post peon.service

%preun
%systemd_preun peon.service

%postun
# Never restart on upgrade: a restart could interrupt a running job.
%systemd_postun peon.service

%files
%license LICENSE
%doc README.md SPEC.md
%{_bindir}/%{name}
%{_unitdir}/peon.service
%dir %attr(0700,root,root) %{_sysconfdir}/peon
%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/peon/config.yaml
%dir %attr(0700,root,root) %{_sharedstatedir}/peon

%changelog
* Thu Oct 29 2026 Tomas Tomecek <tomas@tomecek.net> - 0.1.0-1
- Initial package: daemon, systemd unit
