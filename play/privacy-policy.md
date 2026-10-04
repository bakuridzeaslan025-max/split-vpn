# Политика конфиденциальности Split VPN / Split VPN Privacy Policy

Публичная версия: `privacy-site/index.html` (тот же текст, RU + EN на одной странице).
Дата вступления в силу: 4 октября 2026 г.

---

## Русский

### Политика конфиденциальности приложения Split VPN

Приложение Split VPN (пакет `org.newvpn`) для Android направляет через зашифрованный туннель к серверу разработчика трафик только тех сервисов, которые пользователь отметил в приложении. Весь остальной трафик устройства идёт напрямую и приложением не обрабатывается.

#### 1. Аккаунты и персональные данные

В приложении нет аккаунтов и регистрации. Мы не запрашиваем имя, адрес электронной почты, номер телефона, платёжные данные или другие персональные данные и не собираем их. Используются только технические идентификаторы, не связанные с личностью: идентификаторы установки (п. 3, 5 и 7) и, для статистики использования, хэш идентификатора устройства, который сохраняется при переустановке приложения (п. 7).

#### 2. Что происходит с трафиком

Через туннель проходит трафик только к выбранным сервисам. Внутри туннеля он остаётся зашифрованным самими приложениями и сайтами (например, TLS). Мы не расшифровываем, не читаем, не изменяем и не сохраняем содержимое трафика. Мы не подменяем и не вставляем рекламу и не перенаправляем трафик других приложений в коммерческих целях.

#### 3. Технический журнал на сервере

Сервер не записывает, кто к каким адресам и сервисам подключается. В техническом журнале остаются только служебные события и ошибки: время, случайный идентификатор установки из ключа доступа, а при отклонённых соединениях (превышен лимит, неверный ключ) — IP-адрес клиента. В запись о неудачном соединении может попасть адрес назначения, но без сведений о том, кто его запрашивал. Журнал хранится не дольше 90 дней, не передаётся третьим лицам и не используется для профилирования или рекламы.

#### 4. Данные на устройстве

- **Журнал приложения** — технические события работы туннеля и имена сервисов, к которым устанавливались соединения. Хранится на устройстве; пользователь может сам отправить его разработчику кнопкой «Поделиться логом». Автоматически журнал не отправляется, кроме отрывка в отчёте о сбое (п. 7).
- **Кеш адресов сервисов** — список сетевых адресов выбранных сервисов. Хранится только на устройстве.
- **Ключ доступа к серверу** — выдаётся сервером при первом запуске, хранится в закрытом хранилище приложения и обновляется примерно раз в неделю.

При удалении приложения все эти данные удаляются вместе с ним.

#### 5. Google Play Integrity

Чтобы получить доступ к серверу без регистрации, приложение при первом запуске и примерно раз в неделю запрашивает у сервисов Google Play токен целостности (Play Integrity API) и передаёт его на наш сервер для проверки. Токен подтверждает, что приложение установлено из Google Play на исправном устройстве; он не содержит имени, адреса электронной почты или других персональных данных пользователя и не сохраняется после проверки. Обработка данных на стороне Google описана в политике конфиденциальности Google. На устройствах без сервисов Google вместо этого можно ввести код доступа.

#### 6. Разрешения Android

- **VPN (VpnService)** — создание туннеля для трафика выбранных сервисов. Основная функция приложения.
- **Foreground service и уведомления** — чтобы система не выгружала VPN в фоне и пользователь видел, что он включён.
- **Интернет и состояние сети** — соединение с сервером, переподключение при смене сети.
- **Запрос отключения оптимизации батареи** — по желанию пользователя, чтобы система не останавливала VPN в фоне.
- **Автозапуск после перезагрузки** — восстановление VPN, если он был включён до перезагрузки.

#### 7. Отчёты о сбоях, статистика, настройки и третьи стороны

Для диагностики сбоев приложение использует Firebase Crashlytics (Google). При сбое или внутренней ошибке в Google автоматически отправляется отчёт: стек вызовов, модель устройства, версии Android и приложения, идентификатор установки Firebase, список включённых в приложении сервисов и последние технические строки журнала приложения. Перед отправкой из них удаляются сетевые адреса, а строки с именами сайтов, к которым подключался пользователь, в отчёт не попадают вовсе. Отчёты хранятся до 90 дней и используются только для исправления ошибок; Google обрабатывает их по нашему поручению в соответствии со своей политикой конфиденциальности. Отключить отправку отчётов в приложении нельзя.

Для статистики использования (сколько устройств пользуются VPN, почему не удаётся подключиться) приложение использует Google Analytics для Firebase (Google). Приложение отправляет события трёх видов, каждое не чаще раза в сутки (неудачное подключение — раз в сутки на каждую причину): VPN работал (через сервер прошло хотя бы одно соединение); VPN не удалось включить или сервер недоступен — с причиной из короткого фиксированного списка (например, «нет сети», «нужен код доступа»), без текста ошибки; исчерпан дневной лимит трафика. Кроме того, SDK сам отмечает первый запуск, обновление и удаление приложения и начало сеанса. С событиями передаются: включена ли блокировка рекламы, версии приложения и Android, модель устройства, язык, примерное местоположение (страна, город), которое Google определяет по IP-адресу, случайный идентификатор установки и идентификатор пользователя — усечённый хэш SHA-256 от Android ID. Сам Android ID не передаётся; хэш сохраняется при переустановке приложения и меняется при сбросе устройства к заводским настройкам. Рекламный идентификатор, сетевые адреса, имена сайтов и сервисов, которые открывал пользователь, не передаются; рекламные функции, Google Signals и передача данных Google для его собственных целей отключены. Данные хранятся до 14 месяцев. Отключить статистику в приложении нельзя.

Настройки приложения (список серверов, номер последней версии, адрес страницы загрузки) приходят из Firebase Remote Config (Google): приложение запрашивает их при включении VPN и затем раз в несколько часов. В запросе передаются идентификатор установки Firebase, версии приложения и Android, язык, регион и часовой пояс устройства и тип сборки; сведения о трафике и о том, какие сайты открывал пользователь, не передаются. Кроме Firebase Crashlytics, Google Analytics для Firebase, Firebase Remote Config и Play Integrity API, в приложении нет сторонних SDK; рекламы, рекламных идентификаторов и рекламных трекеров нет. Мы не продаём данные и не передаём их третьим лицам для их собственных целей.

#### 8. Дети

Приложение не предназначено для детей младше 18 лет, и мы сознательно не собираем данные детей.

#### 9. Изменения

При изменении политики мы обновим эту страницу и дату вступления в силу.

#### 10. Контакты

Вопросы по этой политике: bakuridzeaslan025@gmail.com

---

## English

### Split VPN Privacy Policy

Split VPN (package `org.newvpn`) is an Android app that routes traffic of the services the user selects in the app through an encrypted tunnel to the developer's server. All other traffic on the device goes directly and is not handled by the app.

#### 1. Accounts and personal data

The app has no accounts and no sign-up. We do not ask for or collect your name, e-mail address, phone number, payment details or any other personal data. Only technical identifiers that are not linked to your identity are used: installation identifiers (sections 3, 5 and 7) and, for usage statistics, a hash of the device identifier that persists if the app is reinstalled (section 7).

#### 2. What happens to your traffic

Only traffic to the selected services goes through the tunnel. Inside the tunnel it stays encrypted by the apps and websites themselves (for example, TLS). We do not decrypt, read, modify or store the content of your traffic. We do not inject or replace ads and do not redirect traffic of other apps for commercial purposes.

#### 3. Technical log on the server

The server does not record who connects to which addresses or services. Its technical log holds only service events and errors: the time, a random installation identifier from the access key and, for rejected connections (limit exceeded, invalid key), the client IP address. A failed connection may be logged with its destination address, but without any information about who requested it. The log is kept for no longer than 90 days, is not shared with third parties and is not used for profiling or advertising.

#### 4. Data on the device

- **App log** — technical events of the tunnel and the names of services connections were made to. Stored on the device; the user can send it to the developer with the “Share log” button. It is not sent automatically, except for the excerpt included in a crash report (section 7).
- **Service address cache** — a list of network addresses of the selected services. Stored only on the device.
- **Server access key** — issued by the server on first launch, stored in the app's private storage and renewed about once a week.

Uninstalling the app removes all of this data.

#### 5. Google Play Integrity

To get server access without sign-up, on first launch and about once a week the app requests an integrity token from Google Play services (Play Integrity API) and sends it to our server for verification. The token confirms that the app was installed from Google Play on a genuine device; it contains no name, e-mail address or other personal data and is not stored after verification. Google's handling of data is described in Google's privacy policy. On devices without Google services an access code can be entered instead.

#### 6. Android permissions

- **VPN (VpnService)** — creates the tunnel for traffic of the selected services. This is the app's core function.
- **Foreground service and notifications** — so the system keeps the VPN running in the background and the user can see it is on.
- **Internet and network state** — connecting to the server and reconnecting when the network changes.
- **Request to ignore battery optimizations** — optional, at the user's request, so the system does not stop the VPN in the background.
- **Start after reboot** — restores the VPN if it was on before the reboot.

#### 7. Crash reports, usage statistics, settings and third parties

To diagnose crashes the app uses Firebase Crashlytics (Google). When the app crashes or hits an internal error, a report is sent to Google automatically: the stack trace, device model, Android and app versions, the Firebase installation ID, the list of services enabled in the app and the most recent technical lines of the app log. Network addresses are removed from them before sending, and lines naming the sites the user connected to are never included. Reports are kept for up to 90 days and are used only to fix bugs; Google processes them on our behalf under its own privacy policy. Crash reporting cannot be turned off in the app.

For usage statistics (how many devices use the VPN, why connecting fails) the app uses Google Analytics for Firebase (Google). The app sends three kinds of events, each at most once a day (a failed connection — once a day per reason): the VPN worked (at least one connection went through the server); the VPN could not be turned on or the server is unreachable — with a reason from a short fixed list (for example, “no network”, “access code needed”), without the error text; the daily traffic limit was used up. In addition, the SDK itself records the first launch, updates and removal of the app and the start of a session. The events carry: whether ad blocking is on, the app and Android versions, the device model, language, approximate location (country, city) that Google derives from the IP address, a random installation identifier and a user identifier — a truncated SHA-256 hash of the Android ID. The Android ID itself is not sent; the hash persists if the app is reinstalled and changes after a factory reset. The advertising identifier, network addresses and the names of sites and services the user opened are not sent; advertising features, Google signals and data sharing with Google for its own purposes are turned off. The data is kept for up to 14 months. Usage statistics cannot be turned off in the app.

The app's settings (the server list, the latest version number, the download page address) come from Firebase Remote Config (Google): the app requests them when the VPN is turned on and then every few hours. The request carries the Firebase installation ID, the app and Android versions, the device language, region and time zone, and the build type; nothing about the traffic or the sites the user visited is sent. Apart from Firebase Crashlytics, Google Analytics for Firebase, Firebase Remote Config and the Play Integrity API the app contains no third-party SDKs, and no ads, advertising identifiers or ad trackers. We do not sell data or share it with third parties for their own purposes.

#### 8. Children

The app is not intended for children under 18, and we do not knowingly collect data from children.

#### 9. Changes

If this policy changes, we will update this page and the effective date.

#### 10. Contact

Questions about this policy: bakuridzeaslan025@gmail.com
