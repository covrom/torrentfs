#!/bin/bash

go build -ldflags="-s -w" .
fullver=`./torrentfs -version`
ver=`./torrentfs -version|grep -Po '(?<=torrentfs )\d.\d'`
echo $ver

cd ./distr

rm ./*.deb
mkdir -p -m 0755 ./deb/DEBIAN

echo "Package: torrentfs
Version: $ver
Provides: torrentfs
Section: utils
Priority: optional
Architecture: amd64
Maintainer: Roman Covanyan <rs@tsov.pro>
Description: $fullver
 $fullver daemon for systemd Debian-based distribs
" > ./deb/DEBIAN/control
chmod 0644 ./deb/DEBIAN/control

echo "/etc/systemd/system/torrentfs.service" > ./deb/DEBIAN/conffiles
chmod 0644 ./deb/DEBIAN/conffiles

echo "/var/lib/torrentfs" > ./deb/DEBIAN/dirs
chmod 0644 ./deb/DEBIAN/dirs

echo "#!/bin/bash

systemctl daemon-reload
systemctl enable torrentfs.service
systemctl start torrentfs
" > ./deb/DEBIAN/postinst
chmod 0755 ./deb/DEBIAN/postinst

echo "#!/bin/bash

systemctl stop torrentfs
systemctl disable torrentfs
exit 0
" > ./deb/DEBIAN/prerm
chmod 0755 ./deb/DEBIAN/prerm

echo "#!/bin/bash

systemctl daemon-reload
" > ./deb/DEBIAN/postrm
chmod 0755 ./deb/DEBIAN/postrm

mkdir -p -m 0755 ./deb/opt/torrentfs
mkdir -p -m 0755 ./deb/etc/systemd/system
mkdir -p -m 0755 ./deb/var/lib/torrentfs

cp ../torrentfs ./deb/opt/torrentfs/
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

WorkingDirectory=/var/lib/torrentfs/
Restart=always
ExecStart=/opt/torrentfs/torrentfs

[Install]
WantedBy=multi-user.target
" > ./deb/etc/systemd/system/torrentfs.service

chmod 0750 ./deb/opt/torrentfs/torrentfs
chmod 0640 ./deb/etc/systemd/system/torrentfs.service

fakeroot dpkg-deb --build ./deb

mv ./deb.deb "./torrentfs_"$ver"_amd64.deb"
lintian --no-tag-display-limit "./torrentfs_"$ver"_amd64.deb"

rm -r ./deb