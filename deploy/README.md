# Свой сервер

Сайт-заглушка на 443 (nginx + Let's Encrypt), за ним на секретном пути `VPN_PATH` — relay. Всё в Docker. Relay собирается на твоём компьютере в Docker и заливается на сервер бинарником: на слабом VPS Go не собираем.

## Что понадобится

- VPS вне РФ: 1 ядро, 1 ГБ RAM, чистый Debian 12+/Ubuntu 22.04+, доступ `root` по ssh-ключу.
- Домен (подойдёт бесплатный, например DuckDNS), A-запись → IP сервера.
- На своём компьютере: Docker, `make`, `ssh`, `openssl`.

## Установка

Все команды — из `deploy/`.

1. `cp .env.mk.example .env.mk`, вписать `SSH_HOST` (`root@<ip>`) и `REMOTE_DIR` (например `/opt/vpn`).
2. `cp .env.example .env` и заполнить:
   - `DOMAIN` — домен сайта;
   - `VPN_PATH` — `echo "/$(openssl rand -hex 12)"`;
   - `ISSUER_KEY`, `CRED_PUB` — **скопировать с уже работающего сервера**; если сервер первый — `make keygen`.
3. `make setup` — Docker и firewall на сервере (один раз).
4. `make all` — собрать relay, залить файлы и `.env`, поднять контейнеры. Пока сертификата нет, nginx слушает только 80.
5. `make cert` — выпустить сертификат (A-запись уже должна смотреть на сервер). Запуск = согласие с Subscriber Agreement Let's Encrypt. Дальше certbot продлевает сам, nginx перечитывает раз в 12 ч.
6. Проверка: `https://<домен>/` — заглушка, `https://<домен>/что-угодно` — 404.
7. Play Integrity (по желанию): положить JSON-ключ сервисного аккаунта в `REMOTE_DIR/relay/data/sa.json`, затем `make restart`. Без него устройства регистрируются только по invite-коду.

Потом:

- `make relay` — новый бинарник relay: сборка, заливка, перезапуск только relay.
- `make restart` — перезапустить relay (например, после замены `sa.json`).
- `make push up` — после правок `.env`, nginx или заглушки.
- `make invite` — одноразовый код доступа для устройства без Google Play.

## Добавить сервер в список клиента

В `rc/endpoints.json` (gitignored, образец `rc/endpoints.example.json`) добавить объект `{"host": DOMAIN, "ip": IP сервера, "port": 443, "path": VPN_PATH}`, затем `make rc-push` из `android/`. Для дефолта в сборке — та же четвёрка строкой `host|ip|port|path` в `vds.endpoints` в `android/local.properties` (серверы через `;`).

## Ключ issuer

`ISSUER_KEY` (и `CRED_PUB`) — **одинаковые на всех серверах**. Тогда устройство продлевает credential на любом живом сервере, а выданный одним сервером credential принимают все. Другой ключ = клиенты этого сервера не пустят и наоборот. Ключ — секрет: только в `.env`, не в git и не в чаты.

## Безопасность

- Снаружи открыты только ssh, 80 и 443 (`ufw`, ставит `make setup`). ufw не управляет портами Docker: опубликованные контейнерами порты открыты мимо него, поэтому в compose наружу опубликованы только 80/443 у nginx. Relay порт не публикует, до него только через nginx.
- На всё, кроме заглушки и `VPN_PATH`, — 404 nginx. Relay отвечает на мусор тем же 404, так что путь по ответу не угадать.
- `access_log off` на `VPN_PATH`: relay и так не пишет, кто куда ходил.
- TLS без SNI или с чужим SNI обрывается (`ssl_reject_handshake`).
- `.env` на сервере — `600`. Invite-коды, отзывы и `sa.json` лежат в `REMOTE_DIR/relay/data/` (как на боевом, `make invite` из `relay/` тоже работает).
