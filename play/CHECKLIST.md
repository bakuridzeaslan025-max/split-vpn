# Чеклист: что куда вставлять в Play Console

Приложение: **Split VPN**, пакет `org.newvpn`, internal testing, версия 0.3.0 (101). Листинги и графика — в этой папке `play/`, ответы на анкеты и видео — во внутренних документах.

## Требуют твоего решения / действия

1. **Privacy policy URL.** Опубликовано на GitHub Pages: **https://bakuridzeaslan025-max.github.io/split-vpn-privacy/** (репо `bakuridzeaslan025-max/split-vpn-privacy`, с сервером VPN не связан). Альтернатива, если аккаунт/домен не нравится: Cloudflare Pages / Netlify Drop — тот же `privacy-site/index.html`. Обновление: править `privacy-policy.md` → перегенерить `index.html` → push в репо.
2. **Контакт в политике:** bakuridzeaslan025@gmail.com (вписан, опубликован). Тот же адрес поставь в Store settings → Contact email.
3. **VPN-декларация: «собираем данные через VpnService?»** — вариант A («No») или B («Yes» + in-app disclosure, доработка кода). Подробно — во внутренних документах.
4. **Data safety: отмечать ли «Web browsing history»** дополнительно к Diagnostics. Рекомендация — нет.
5. **Foreground service: перейти на `systemExempted`** (декларация не нужна, но правка манифеста + проверка на телефоне) или оставить `specialUse` и заполнить форму.
6. **App access:** «All functionality is available without special access». Код доступа — только запасной путь при отказе ревью.
7. **Видео** — записано с эмулятора, лежит во внутренних документах (включение → «Подключено» → шторка с уведомлением «Split VPN — Tunnel active»). Залить на YouTube как unlisted, ссылку в форму FGS и в VPN-декларацию. Для записи уведомление включено через `pm grant POST_NOTIFICATIONS`: у пользователей на Android 13+ оно не видно, пока приложение не запросит разрешение (TODO).
8. **Возраст**: рекомендую только 18+.
9. ~~Скриншот «Держать VPN включённым»~~ — layout починен (insets в `KeepAliveActivity`, как в `MainActivity`), переснят 2026-09-15.
10. **Иконка 512**: в `res/` PNG нет, только adaptive vector. `graphics/icon-512.png` отрендерена из него (фон #1565C0 + тот же глиф, внутренние 72dp). В консоли иконка уже есть — заменять не обязательно.

## Store listing → Main store listing

| Поле | Откуда |
|---|---|
| Default language | English (en-US) — уже стоит в консоли, оставить; русским пользователям показывается перевод ru-RU |
| App name | `listing-en.md` → Title (сейчас в консоли «VPN», заменить на «Split VPN») |
| Short description | `listing-en.md` |
| Full description | `listing-en.md` |
| Manage translations → Add → Russian (ru-RU) | те же три поля из `listing-ru.md` |
| App icon 512×512 | `graphics/icon-512.png` (или оставить текущую) |
| Feature graphic 1024×500 | en-US: `graphics/feature-en.png`; ru-RU: `graphics/feature-ru.png` |
| Phone screenshots (2–8) | en-US: `graphics/screenshots/en/`; ru-RU: `graphics/screenshots/ru/` (1080×1920 PNG, 9:16). 5 кадров в порядке имён: `01-services`, `02-split` (схема «через VPN / напрямую»), `03-connected`, `04-autostart`, `06-log` |
| 7-inch / 10-inch tablet | не обязательны |
| Video | не обязательно |

## Store listing → Store settings

| Поле | Значение |
|---|---|
| App category | Application → **Tools** |
| Tags | VPN, Tools |
| Contact email | адрес разработчика (обязателен; на него ссылается политика) |

## Release → Internal testing → Release notes

`listing-ru.md` / `listing-en.md` → «Заметки о выпуске» (`<ru-RU>` и `<en-US>`).

## App content (Policy → App content)

| Раздел | Что вставить | Где |
|---|---|---|
| Privacy policy | URL из п. 1 | — |
| Ads | No | внутренние документы |
| App access | All functionality available | внутренние документы |
| Content rating | анкета IARC, категория Utility, все No | внутренние документы |
| Target audience and content | 18+ | внутренние документы |
| News apps | No | внутренние документы |
| COVID-19 | No | внутренние документы |
| Data safety | Yes collected → Diagnostics, not shared, required, App functionality | внутренние документы |
| Government apps | No | внутренние документы |
| Financial features | none | внутренние документы |
| Health | none | внутренние документы |
| VPN (VpnService) | core = Yes, видео, данные — по решению п. 3, monetization = No | внутренние документы |
| Foreground service permissions | specialUse → форма + видео; после перехода на systemExempted раздел не появится | внутренние документы |
| Photo and video permissions / Advertising ID / Sensitive permissions | не используются → No / не появятся |

## Перед выходом из internal testing (в этот пакет не входит)

- Уведомление VPN с «Выключить» + запрос `POST_NOTIFICATIONS`.
- Обфускация R8, хранить `mapping.txt`.
- Перевыпустить ключ сервисного аккаунта Integrity.
- Если выбран вариант B в п. 3 — экран disclosure.

## Проверено глазами модератора

Тексты листинга, политика и декларации: нет слов «блокировка/обход/цензура», нет страны сервера, доменов и адресов, нет обещаний, которые нельзя подтвердить; VpnService задокументирован в листинге (абзац «О VPN»); серверный лог одинаково описан в политике, Data safety и декларации.
