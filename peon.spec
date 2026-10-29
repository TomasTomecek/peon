Name:           peon
Version:        0.1.0
Release:        1%{?dist}
Summary:        Run human-approved commands from Forgejo issues

License:        MIT
URL:            https://github.com/TomasTomecek/peon
# Tarball with vendored Go modules, produced by packaging/create-archive.sh
# (Packit runs it for every build; for releases, attach its output).
Source0:        %{name}-%{version}.tar.gz

# Go does not support every architecture Fedora builds for.
ExclusiveArch:  %{golang_arches}

BuildRequires:  golang >= 1.22
BuildRequires:  systemd-rpm-macros
Requires:       polkit
Requires:       systemd
Requires(pre):  shadow-utils
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
export GOFLAGS="-mod=vendor -trimpath"
go build -buildmode=pie \
    -ldflags "-B gobuildid -X main.version=%{version}-%{release}" \
    -o %{name} ./cmd/peon

%check
export GOFLAGS="-mod=vendor"
go test ./...

%install
install -Dpm0755 %{name} %{buildroot}%{_bindir}/%{name}
install -Dpm0644 dist/peon.service %{buildroot}%{_unitdir}/peon.service
install -Dpm0644 dist/peon.sysusers %{buildroot}%{_sysusersdir}/peon.conf
install -Dpm0644 dist/50-peon.rules %{buildroot}%{_datadir}/polkit-1/rules.d/50-peon.rules
install -Dpm0640 dist/config.toml %{buildroot}%{_sysconfdir}/peon/config.toml
install -dm0750 %{buildroot}%{_sharedstatedir}/peon

%pre
# Created here as well as via sysusers.d so the user exists before the
# files owned by it are laid down, on every supported release.
getent group peon >/dev/null || groupadd -r peon
getent passwd peon >/dev/null || \
    useradd -r -g peon -d %{_sharedstatedir}/peon -s /usr/sbin/nologin \
            -c "peon daemon" peon
exit 0

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
%{_sysusersdir}/peon.conf
%{_datadir}/polkit-1/rules.d/50-peon.rules
%dir %attr(0750,root,peon) %{_sysconfdir}/peon
%config(noreplace) %attr(0640,root,peon) %{_sysconfdir}/peon/config.toml
%dir %attr(0750,peon,peon) %{_sharedstatedir}/peon

%changelog
* Thu Oct 29 2026 Tomas Tomecek <nereone@gmail.com> - 0.1.0-1
- Initial package: config validation, systemd unit, polkit rule
