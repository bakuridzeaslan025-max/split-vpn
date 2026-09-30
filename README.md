# Split VPN

[![Downloads](https://img.shields.io/github/downloads/bakuridzeaslan025-max/split-vpn/total)](https://github.com/bakuridzeaslan025-max/split-vpn/releases)

*Split-tunnel VPN for Android. Only the blocked services you pick (Telegram, YouTube, Instagram, X, …) go through your own server abroad; everything else stays direct, at full speed and with local addresses. The relay hides behind an ordinary website on 443. GPL-3.0. The rest of this README is in Russian.*

Приложение для Android, которое пускает через VDS только выбранные заблокированные сервисы. Остальной трафик идёт напрямую: банки, Госуслуги и маркетплейсы видят российский IP, скорость не страдает, батарея почти не тратится.

## Как устроено

- **Клиент.** `VpnService` заворачивает в TUN только подсети выбранных сервисов. Внутри — Go-библиотека (`android/tunnel/`, userspace TCP-стек из gVisor): на каждое TCP-соединение открывается TLS к relay. Соединения поднимаются по требованию, постоянного канала и keepalive нет.
- **Сервер.** nginx с обычным сайтом и relay (`relay/`) за ним, который пересылает байты к нужному адресу.
- **Split DNS.** Имена выбранных сервисов резолвятся через DoH (Cloudflare) сквозь relay, остальные — обычным резолвером сети.
- **Remote Config.** Список серверов и номер свежей версии приходят из Firebase Remote Config, поэтому серверы можно менять без выпуска новой версии.

## Установка

Скачать и прочитать, как поставить: [страница загрузки](https://bakuridzeaslan025-max.github.io/split-vpn/). Бери APK только с неё или из [Releases](https://github.com/bakuridzeaslan025-max/split-vpn/releases). Если APK получен из вторых рук, сверь подпись — у настоящего APK SHA-256 сертификата такой:

```
4cf035d2f09dab9f1781d6e5af593b7d5ef59dea15b45adfe4a596027fd2f8c4
```

Проверка (`apksigner` из Android SDK, `build-tools/<версия>/`):

```sh
apksigner verify --print-certs split-vpn-X.Y.Z.apk | grep SHA-256
shasum -a 256 -c split-vpn-X.Y.Z.apk.sha256   # целостность файла
```

Отпечаток другой — не ставь.

APK собирает и подписывает мейнтейнер у себя, ключ подписи в CI не попадает. Побайтно сборка не воспроизводима: R8/AGP и Go-сборка (без `-trimpath`) дают разные байты, а `rc.key` и список серверов есть только у мейнтейнера. Поэтому доверие — к отпечатку подписи.

## Обновления

Приложение само узнаёт о новой версии и показывает плашку; «Обновить» открывает страницу загрузки, APK ставится поверх вручную. Сами пакеты приложение не ставит (нет права `REQUEST_INSTALL_PACKAGES`). После обновления VPN поднимается сам, если был включён.

## Регистрация

Сервер пускает только зарегистрированные устройства. Регистрация автоматическая, через Play Integrity: нужен Google Play и «честное» устройство (без root, с заблокированным загрузчиком, стоковая прошивка). Устройство без Google Play регистрируется одноразовым кодом приглашения, который выдаёт владелец сервера (`make invite`).

## Свой сервер

Шаблон — [`deploy/`](deploy/README.md): VPS, домен, Docker, пара команд `make`. Свой сервер добавляется в сборку (`vds.endpoints`) или в Remote Config своего Firebase-проекта. Своя сборка на своём сервере регистрируется только кодом приглашения: Play Integrity пропускает лишь APK с нашей подписью.

## Сборка из исходников

Нужны Docker, `make`, JDK 17+ и Android SDK. Go-часть собирается в Docker-образе с запиненными Go, NDK и gomobile.

```sh
cd android
echo "sdk.dir=$HOME/Library/Android/sdk" > local.properties   # или export ANDROID_HOME=…, обязательно
cp app/google-services.stub.json app/google-services.json  # если нет своего из Firebase
make tunnel                 # AAR с Go-туннелем → app/libs/tunnel.aar
./gradlew assembleDebug     # или make apk
make test                   # Go (-race), rc/tool, Kotlin unit
make -C ../relay test
make itest                  # инструментальные, только на эмуляторе
```

`android/app/google-services.json` в репо нет: свой из Firebase (пакет `org.newvpn`) или заглушка `google-services.stub.json` — с ней Crashlytics и Remote Config просто не работают.

`android/local.properties` (в git не попадает), обязателен только `sdk.dir` (или `ANDROID_HOME`):

| Ключ | Зачем |
|---|---|
| `sdk.dir` | путь к Android SDK |
| `rc.key` | 64 hex, AES-ключ списка серверов (им же шифрует Remote Config) |
| `vds.endpoints` | встроенный список серверов (до первого ответа Remote Config): `host\|ip\|port\|path`, через `;` |
| `integrity.project` | номер Cloud-проекта для Play Integrity; без него — только коды приглашения |
| `keystore.file`, `keystore.pass`, `key.alias`, `key.pass` | ключ подписи |
| `rc.sa` | ключ сервисного аккаунта для `make rc-push` и др. |
| `vds.ssh` | `user@host` для `./gradlew updateRoutes` |

Без `rc.key`/`vds.endpoints` debug-сборка собирается, но серверов не знает; release-сборка без них не собирается. Своя сборка подписана другим ключом, поэтому Play Integrity на чужих серверах её не пропустит и поверх релизной версии она не встанет.

## Модель угроз

Это инструмент для обхода блокировок, не для анонимности.

- **Маскировка** трафика и сервера рассчитана на автоматические сканеры и DPI, а не на целевой анализ: код открыт, и кто прочитает его и распакует APK, узнает, как всё устроено. Белые списки на мобильном интернете приложение не обходит.
- **Провайдер видит** IP сервера, домен сайта (SNI), объём и время трафика. Содержимое зашифровано.
- **Relay видит** IP устройства, адреса и порты, куда идут соединения через туннель, объём и время. Содержимое HTTPS ему не видно. Журнала успешных соединений relay не ведёт; в лог попадают регистрации, ошибки сессий (с ID credential, иногда с адресом назначения) и посторонние запросы к сайту. Это код сервера, а не гарантия: его владельцу приходится доверять. В credential — случайный ID, без аккаунта и почты.
- **DNS.** Имена выбранных сервисов видит Cloudflare (DoH идёт через relay), остальные — резолвер твоей сети, как без VPN.
- **Google.** Remote Config и Play Integrity обращаются к Google напрямую. Crashlytics отправляет крэши и неожиданные ошибки с последними строками журнала приложения, моделью устройства и версией Android. Адреса серверов и credential перед отправкой заменяются заглушками. Строки с доменами, SNI и маршрутами (по сути история посещений) в Crashlytics не уходят никогда — только в журнал на устройстве, который ты можешь отправить сам через «Поделиться».

## Лицензия

[GPL-3.0](LICENSE).
