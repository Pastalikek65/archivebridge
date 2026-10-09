# Türkçe hızlı başlangıç

Bu sayfa ArchiveBridge 1.0 komutlarını anlatır. Hangi sürümlerin indirilebilir olduğunu, paket SHA-256 değerlerini ve o sürüme ait doğrulama kanıtlarını [Releases sayfasından](https://github.com/Pastalikek65/archivebridge/releases) kontrol et. Kanıtlar yalnızca aynı sürüm ve platform için geçerlidir. Önce `archivebridge --version --json` ile kullandığın ikili dosyanın sürümünü doğrula.

Takeout parçalarını seçerek yeni bir plan oluştur, sonra yeni bir dizine aktar ve doğrula:

```sh
./archivebridge plan --source parca-1.zip --source parca-2.zip --output plan.json
./archivebridge export --plan plan.json --out fotograf-arsivi
./archivebridge verify --archive fotograf-arsivi
./archivebridge serve --archive fotograf-arsivi
```

Windows PowerShell'de `./archivebridge` yerine `.\archivebridge.exe` kullan. Boşluk içeren yolları tırnak içine al. Yerel arayüz `http://127.0.0.1:4175` adresinde açılır; Ctrl+C ile durdurulur.

Aktarmadan önce `inspect --source ...` ile içeriği inceleyebilirsin. `compare --plan plan.json --archive fotograf-arsivi` özgün kaynak parçalarını yeniden okur; bu parçalar planın kaydettiği konumlarda erişilebilir olmalıdır. Kesilen dışa aktarmayı `resume --plan plan.json --out fotograf-arsivi` ile sürdür. Mevcut dosyalar tekrar kullanımdan önce doğrulanır. Kaynaklar silinmez; aynı baytlara sahip tekrarlı öğeler tek içerik dosyası kullansa da tüm kaynak görünümleri ve albüm ilişkileri manifestte korunur.

## Immich'e aktarım

Immich bağdaştırıcısı ArchiveBridge 1.0.0 veya üstünü ve tam olarak Immich Server 3.3.1 sürümünü gerektirir. Önce yerel planı oluştur:

```sh
./archivebridge immich plan --archive fotograf-arsivi --json
```

Varsayılan olarak tarihi eksik, bozuk, belirsiz veya çakışan öğeler planı engeller. `--skip-unresolved` açıkça seçilirse bu öğeler aktarılmaz ve raporda atlananlar olarak gösterilir. API anahtarını varsayılan `ARCHIVEBRIDGE_IMMICH_API_KEY` ortam değişkeninde tut; komut satırına yazma.

```sh
./archivebridge immich import --archive fotograf-arsivi --server https://immich.example --report immich-aktarim.json
./archivebridge immich verify --archive fotograf-arsivi --server https://immich.example --report immich-aktarim.json --output immich-dogrulama.json
```

Bağdaştırıcı değişiklik yapmadan önce sunucu sürümünü, hesabı ve gerekli API anahtarı izinlerini denetler. Yedi izin şunlardır: `asset.upload`, `asset.read`, `asset.download`, `album.read`, `album.create`, `albumAsset.create` ve `user.read`. `asset.update` ve içerik silme izinleri istenmez. HTTPS kullan. HTTP yalnızca `--allow-http-loopback` seçeneğiyle ve `127.0.0.1` veya `::1` gibi gerçek loopback IP adreslerinde kullanılabilir.

Takeout özgün dosya sistemi değiştirilme zamanını sağlamaz. Hazır her yükleme özgün medya baytlarıyla birlikte, bilinen Takeout UTC tarihini taşıyan üretilmiş minimal, yalnızca tarih içeren bir XMP sidecar gönderir. Bu tarih Immich'in zorunlu `fileCreatedAt` ve `fileModifiedAt` alanlarında kullanılır; özgün dosya sistemi zamanı geri kazanılmış olmaz. Immich 3.3.1 metadata çalışanı işlemi tamamladıktan sonra `fileCreatedAt`, `fileModifiedAt` ve `ExifDateTimeOriginal` kaynak tarihiyle karşılaştırılır ve özgün medya baytları doğrulanır. Bu kontroller bitmeden yükleme hazır sayılmaz ve albüm değişiklikleri başlamaz. Mevcut bir öğenin tarihi çakışıyorsa metadata değiştirilmeden işlem reddedilir. Plan ve raporlardaki `dateTransferPolicy` değeri `takeout-date-authoritative-generated-xmp-v1` şeklindedir.

İptal veya sınırlı metadata bekleme süresi Immich yüklemeyi kabul ettikten sonra gerçekleşirse kısmi rapor kabul edilmiş uzak öğenin kimliğini korur. Bu durumu yalnızca açık `--resume` ile uzlaştır; eski raporu değiştirme. Takeout'ın ham JSON sidecar dosyaları yerel arşivde kalır; açıklama ve GPS alanları Immich'e eşlenmez. Özgün medya baytları ve gömülü EXIF değişmez.

Kesilen işlemi sürdürmek için önceki raporu oku ve yeni bir rapor yolu ver; eski rapor değiştirilmez:

```sh
./archivebridge immich import --archive fotograf-arsivi --server https://immich.example --resume eski-aktarim.json --report yeni-aktarim.json
```

Sürdürme aynı arşiv manifestini, sunucu sürümünü ve doğrulanmış hesabı gerektirir. Raporlar kaynak adları, albüm ayrıntıları, sunucu adresi, hesap kimliği ve işlem durumunu içerdiğinden gizli tutulmalıdır; API anahtarını içermezler. `immich verify` sunucuda değişiklik yapmaz. Bağdaştırıcı tüm hesabın aktarıldığını iddia etmez ve uzaktaki içerikleri silen bir komut sunmaz. Diğer sınırlamalar için [komut kılavuzuna](cli.md) ve [desteklenen biçimlere](support.md) bak.
