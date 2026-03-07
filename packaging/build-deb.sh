#!/bin/bash
# Собирает .deb пакет для adbtest.
# Использование: bash packaging/build-deb.sh [version]
# Версия берётся из аргумента, GITHUB_REF_NAME или последнего git-тега.
set -euo pipefail

BINARY=${BINARY:-adbtest}
ARCH=${ARCH:-amd64}
APK=${APK:-}

# Определяем версию
VERSION=${1:-${GITHUB_REF_NAME:-}}
if [ -z "$VERSION" ]; then
    VERSION=$(git describe --tags --abbrev=0 2>/dev/null || echo "0.0.0")
fi
VERSION=${VERSION#v}  # убираем ведущий 'v'

PKG="adbtest_${VERSION}_${ARCH}"
echo "Building ${PKG}.deb ..."

# Создаём структуру пакета
rm -rf "${PKG}"
install -Dm755 "${BINARY}"                         "${PKG}/usr/local/bin/adbtest"
# Подставляем версию в TEST_IMAGE (latest → vX.Y.Z)
sed "s|rrawwwrrr/adbtest-tests:latest|rrawwwrrr/adbtest-tests:v${VERSION}|g" \
    packaging/adbtest.env > "${PKG}/etc/default/adbtest"
chmod 644 "${PKG}/etc/default/adbtest"
install -Dm644 packaging/adbtest.service           "${PKG}/etc/systemd/system/adbtest.service"
install -dm755                                     "${PKG}/var/lib/adbtest/apk"
install -dm755                                     "${PKG}/var/lib/adbtest/reports/logs"

# Включаем APK в пакет если передан через переменную APK=...
if [ -n "${APK}" ] && [ -f "${APK}" ]; then
    echo "→ Bundling APK: ${APK}"
    install -Dm644 "${APK}" "${PKG}/var/lib/adbtest/apk/$(basename "${APK}")"
fi

# DEBIAN/control
mkdir -p "${PKG}/DEBIAN"
cat > "${PKG}/DEBIAN/control" <<EOF
Package: adbtest
Version: ${VERSION}
Section: utils
Priority: optional
Architecture: ${ARCH}
Depends: docker.io | docker-ce, adb
Maintainer: rrawwwrrr
Description: ADB Test Runner с веб-дашбордом
 Следит за подключёнными Android-устройствами, запускает Appium
 и тестовые контейнеры в Docker, сохраняет результаты в SQLite
 и показывает их в браузере на порту 9080.
EOF

# DEBIAN/postinst — выполняется после установки
cat > "${PKG}/DEBIAN/postinst" <<'EOF'
#!/bin/bash
set -e
systemctl daemon-reload
systemctl enable adbtest

echo ""
echo "╔══════════════════════════════════════════════════════════╗"
echo "║              adbtest успешно установлен                  ║"
echo "╠══════════════════════════════════════════════════════════╣"
echo "║  1. Отредактируй конфиг:                                 ║"
echo "║     nano /etc/default/adbtest                            ║"
echo "║                                                          ║"
echo "║  2. Запусти сервис:                                      ║"
echo "║     systemctl start adbtest                              ║"
echo "║                                                          ║"
echo "║  3. Логи:  journalctl -u adbtest -f                      ║"
echo "║  4. Дашборд: http://<ip>:9080                            ║"
echo "╚══════════════════════════════════════════════════════════╝"
echo ""
EOF
chmod 755 "${PKG}/DEBIAN/postinst"

# DEBIAN/prerm — выполняется перед удалением
cat > "${PKG}/DEBIAN/prerm" <<'EOF'
#!/bin/bash
set -e
systemctl stop adbtest    2>/dev/null || true
systemctl disable adbtest 2>/dev/null || true
EOF
chmod 755 "${PKG}/DEBIAN/prerm"

# Собираем пакет
dpkg-deb --build --root-owner-group "${PKG}"
rm -rf "${PKG}"

echo "Готово: ${PKG}.deb"
