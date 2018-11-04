#!/bin/bash

go build -ldflags="-s -w" -o "./torrentnotify" .
fullver=`./torrentnotify -version`
ver=`./torrentnotify -version|grep -Po '(?<=torrentnotify )\d.\d'`
echo $ver

cd ./distr

rm ./*.deb
mkdir -p -m 0755 ./deb/DEBIAN

echo "Package: torrentnotify
Version: $ver
Provides: torrentnotify
Section: utils
Priority: optional
Architecture: amd64
Maintainer: Roman Covanyan <rs@tsov.pro>
Description: $fullver
 $fullver daemon for systemd Debian-based distribs
" > ./deb/DEBIAN/control
chmod 0644 ./deb/DEBIAN/control

echo "/etc/systemd/system/torrentnotify.service" > ./deb/DEBIAN/conffiles
chmod 0644 ./deb/DEBIAN/conffiles

echo "/var/lib/torrentnotify" > ./deb/DEBIAN/dirs
chmod 0644 ./deb/DEBIAN/dirs

echo "#!/bin/bash

systemctl daemon-reload
systemctl enable torrentnotify.service
systemctl start torrentnotify
" > ./deb/DEBIAN/postinst
chmod 0755 ./deb/DEBIAN/postinst

echo "#!/bin/bash

systemctl stop torrentnotify
systemctl disable torrentnotify
exit 0
" > ./deb/DEBIAN/prerm
chmod 0755 ./deb/DEBIAN/prerm

echo "#!/bin/bash

systemctl daemon-reload
" > ./deb/DEBIAN/postrm
chmod 0755 ./deb/DEBIAN/postrm

mkdir -p -m 0755 ./deb/opt/torrentnotify
mkdir -p -m 0755 ./deb/etc/systemd/system
mkdir -p -m 0755 ./deb/var/lib/torrentnotify

cp ../torrentnotify ./deb/opt/torrentnotify/
echo "[Unit]
Description=$fullver daemon service
After=network.target local-fs.target network-online.target
Requires=network-online.target

[Service]
LimitMEMLOCK=infinity
LimitNOFILE=65535
Type=simple

#User=nouser
#Group=nogroup

WorkingDirectory=/var/lib/torrentnotify/
Restart=always
ExecStart=/opt/torrentnotify/torrentnotify

[Install]
WantedBy=multi-user.target
" > ./deb/etc/systemd/system/torrentnotify.service

chmod 0755 ./deb/opt/torrentnotify/torrentnotify
chmod 0644 ./deb/etc/systemd/system/torrentnotify.service

fakeroot dpkg-deb --build ./deb

mv ./deb.deb "./torrentnotify_"$ver"_amd64.deb"
lintian --no-tag-display-limit "./torrentnotify_"$ver"_amd64.deb"

rm -r ./deb