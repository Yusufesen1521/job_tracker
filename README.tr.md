# job_tracker

> English documentation: [README.md](README.md)

Gmail'i periyodik okuyup iş başvurusu maillerini bir LLM ile sınıflandıran,
SQLite'a kaydeden, web arayüzünde tablo halinde gösteren ve durum değişiminde
[ntfy.sh](https://ntfy.sh) ile telefona bildirim gönderen, Go ile yazılmış
lokal/VPS dostu bir otomasyon.

- **Dil:** Go 1.22+ (standart kütüphane ağırlıklı, minimum bağımlılık)
- **DB:** SQLite — saf Go driver (`modernc.org/sqlite`), CGO gerekmez
- **Mail:** Resmi Gmail API + OAuth2 (IMAP değil)
- **Sınıflandırma:** Takılabilir `Classifier` interface'i — Google Gemini
  (ücretsiz katman), OpenRouter (ücretsiz modeller) veya Anthropic Claude
- **Ön eleme:** Anahtar kelime filtresi, istenirse küçük ücretsiz bir LLM
  destekli (hibrit mod)
- **Bildirim:** ntfy.sh, `Notifier` interface'i arkasında
- **Arayüz:** `net/http` + `html/template`, tek sayfa sunucu-render tablo

> ⚠️ **Güvenlik:** Bu repo public'tir. Hiçbir secret commit edilmez.
> `.env`, `credentials.json`, `token.json`, `*.db` dosyaları `.gitignore`'dadır.

## Paket yapısı

```
cmd/server        web arayüzü (+ opsiyonel Basic Auth)
cmd/poller        Gmail çek → sınıflandır → DB → bildirim (--once veya ticker)
internal/config   env'den config okuma (.env desteği)
internal/gmail    Gmail API client, OAuth, ön-filtre
internal/classifier  LLM sınıflandırıcılar (interface + Gemini/OpenRouter/Anthropic + prompt)
internal/store    SQLite şema + repository (applications tablosu)
internal/notifier ntfy bildirim (interface + impl)
```

## Kurulum

### 0. Gereksinimler
- Go 1.22+
- Bir Google hesabı (Gmail) ve en az bir LLM API anahtarı
  (Gemini'nin ücretsiz katmanı yeterli: https://aistudio.google.com/apikey)

### 1. Bağımlılıkları al

```bash
go mod download
```

### 2. Gmail API kimlik bilgileri (credentials.json)

1. [Google Cloud Console](https://console.cloud.google.com/) → yeni bir proje oluştur.
2. **APIs & Services → Library** → "Gmail API" → **Enable**.
3. **APIs & Services → OAuth consent screen** → External seç, uygulama adını gir,
   **Test users** kısmına kendi Gmail adresini ekle (yayınlamaya gerek yok).
4. **APIs & Services → Credentials → Create Credentials → OAuth client ID** →
   Application type: **Desktop app** → oluştur.
5. İndirilen JSON dosyasını proje köküne **`credentials.json`** olarak kaydet.
   (Bu dosya `.gitignore`'dadır; asla commit etme.)

### 3. Konfigürasyon (.env)

```bash
cp .env.example .env
```

Değerleri doldur — en azından `GEMINI_API_KEY` (veya başka bir sağlayıcının
anahtarı + `LLM_PROVIDER`). Tüm değişkenler `.env.example` içinde açıklamalıdır.

### 4. OAuth token üret (token.json)

İlk kez bir kez çalıştır:

```bash
go run ./cmd/poller --auth
```

Komut bir URL basar. Tarayıcıda aç, Gmail hesabınla yetki ver; kod lokal bir
callback sunucusu tarafından otomatik yakalanır ve **`token.json`** kaydedilir
(refresh token içerir; `.gitignore`'dadır).

### 5. ntfy.sh bildirimi (opsiyonel)

1. Telefonuna **ntfy** uygulamasını kur (Android/iOS) veya https://ntfy.sh aç.
2. Tahmin edilmesi zor bir topic adı seç (örn. `job-tracker-x7f2k9`).
3. `.env` içinde `NTFY_TOPIC=https://ntfy.sh/job-tracker-x7f2k9` olarak ayarla.
4. Uygulamada aynı topic'e abone ol. Boş bırakırsan bildirim gönderilmez.

## Lokal çalıştırma

```bash
# Bir kez çek-işle (test/cron için):
go run ./cmd/poller --once

# Sürekli çalış (POLL_INTERVAL aralığıyla):
go run ./cmd/poller

# LLM API sağlık/kota kontrolü:
go run ./cmd/poller --check

# Web arayüzü:
go run ./cmd/server
# → http://localhost:8080
```

## LLM sağlayıcıları ve kotalar

Sınıflandırıcı `LLM_PROVIDER` ile seçilir (`gemini` | `openrouter` | `anthropic`).
Tüm sağlayıcılar aynı `Classifier` interface'ini uygular.

- **Gemini** (varsayılan): `gemini-3.1-flash-lite` ücretsiz katmanda dakikada
  15 / günde 500 istek sunar. Dakikalık (RPM) limit aşımlarında API'nin
  önerdiği süre beklenerek otomatik yeniden denenir.
- **OpenRouter**: `:free` sonekli modeller ücretsizdir. Kredisiz hesaplarda
  günde ~50 istek (10$+ kredi ile 1000/gün).
- **Anthropic**: ücretli; ekonomik seçenek Haiku'dur.

Günlük kota dolduğunda poller döngüyü açık bir hatayla durdurur; işlenmemiş
mailler sonraki turda ele alınır. Kotanın bittiğini önceden fark etmek için:

```bash
go run ./cmd/poller --check                       # tek ucuz canlı istek
LIVE_API_TEST=1 go test ./internal/classifier/ -v  # tüm sağlayıcılar için canlı test
```

## Sınıflandırma promptu

Sınıflandırma talimatı `internal/classifier/prompt.go` içindedir. LLM
davranışını değiştirmek için yalnızca o dosyayı düzenlemen yeterli.

Model çıktısı şu JSON şemasında beklenir:

```json
{"is_job_related": true, "company": "Acme", "status": "interview", "confidence": 0.92}
```

`is_job_related=false` veya `confidence < CONFIDENCE_THRESHOLD` ise kayıt
DB'ye yazılmaz.

## Ön-filtre

LLM'e gitmeden önce ucuz bir eleme yapılır (`internal/gmail/filter.go`):

- `FILTER_EXCLUDE_DOMAINS`'teki domainlerden gelenler doğrudan elenir.
- `FILTER_KEYWORDS`'ten biri konu/gövdede geçiyorsa mail geçer
  (liste boşsa filtre kapalı).
- **Hibrit mod** (`PREFILTER_MODE=hybrid`): anahtar kelime eşleşmeyen mailler
  OpenRouter üzerindeki küçük ücretsiz bir LLM'e (`PREFILTER_MODEL`) sorulur;
  YES/NO cevabına göre geçer veya elenir. Anahtar kelime listesini atlatan
  başvuru mailleri böylece yakalanır; maliyeti birkaç ücretsiz katman isteğidir.

## Test

```bash
go test ./...
```

Birim testleri mock HTTP sunucuları kullanır, API kotası tüketmez. Canlı API
testleri `LIVE_API_TEST=1` ile isteğe bağlı çalışır.

## VPS deploy

1. **Build** (Linux hedefi için, Windows/Mac'ten):

   ```bash
   GOOS=linux GOARCH=amd64 go build -o bin/poller ./cmd/poller
   GOOS=linux GOARCH=amd64 go build -o bin/server ./cmd/server
   ```

   `modernc.org/sqlite` saf Go olduğu için CGO/cross-compile derdi yok.

2. **Secret'ları güvenli taşı** (asla repoyla değil — `scp` ile):

   ```bash
   scp .env credentials.json token.json user@vps:/opt/job_tracker/
   scp bin/poller bin/server user@vps:/opt/job_tracker/
   ```

   `token.json` refresh token içerdiği için VPS'te headless çalışır; tekrar
   tarayıcı gerekmez.

3. **Poller'ı zamanla** — iki seçenek:

   **a) systemd timer + `--once`** (önerilen, cron benzeri):

   `/etc/systemd/system/job-poller.service`:
   ```ini
   [Unit]
   Description=Job Tracker Poller
   [Service]
   WorkingDirectory=/opt/job_tracker
   ExecStart=/opt/job_tracker/poller --once
   EnvironmentFile=/opt/job_tracker/.env
   ```
   `/etc/systemd/system/job-poller.timer`:
   ```ini
   [Unit]
   Description=Run job poller every 15m
   [Timer]
   OnBootSec=2m
   OnUnitActiveSec=15m
   [Install]
   WantedBy=timers.target
   ```
   ```bash
   sudo systemctl enable --now job-poller.timer
   ```

   **b) Sürekli servis (kendi ticker'ı):** `ExecStart=/opt/job_tracker/poller`
   (flag'siz), `Restart=always` ekleyip normal bir `.service` olarak çalıştır.

4. **Web arayüzü servisi** `/etc/systemd/system/job-server.service`:
   ```ini
   [Unit]
   Description=Job Tracker Web
   [Service]
   WorkingDirectory=/opt/job_tracker
   ExecStart=/opt/job_tracker/server
   EnvironmentFile=/opt/job_tracker/.env
   Restart=always
   [Install]
   WantedBy=multi-user.target
   ```

5. **Web güvenliği:** Arayüz internete açıksa `.env`'de `WEB_USER`/`WEB_PASS`
   doldur (HTTP Basic Auth aktif olur). Ek olarak TLS için önüne bir reverse
   proxy (Caddy/Nginx) koymanı öneririz.

## Lisans

Kişisel kullanım için. Secret yönetimine dikkat: bu repo public'tir.
