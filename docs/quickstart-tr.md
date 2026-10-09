# Türkçe hızlı başlangıç

ArchiveBridge, seçtiğin Google Photos Takeout parçalarını yerel bir arşive taşır. Orijinal dosyaları değiştirmez; fotoğraf tarihlerini, metadata eşleşmelerini ve albüm ilişkilerini bir manifestte saklar. Google hesabına giriş veya ücretli hizmet gerekmez.

1. Sürüm paketini indir, SHA-256 değerini kontrol et ve paketi aç.
2. Tüm ilgili Takeout parçalarını `--source` ile seçerek planı oluştur.
3. Yeni bir dizine aktar, ardından doğrula.
4. Yerel arayüzü başlat ve albümleri incele.

```sh
./archivebridge plan --source parca-1.zip --source parca-2.zip --output plan.json
./archivebridge export --plan plan.json --out fotograf-arsivi
./archivebridge verify --archive fotograf-arsivi
./archivebridge serve --archive fotograf-arsivi
```

Windows'ta `./archivebridge` yerine `.\archivebridge.exe` kullan. Boşluk içeren yolları tırnak içine al. Arayüz `http://127.0.0.1:4175` adresinde açılır; Ctrl+C ile kapanır.

Çakışan metadata sessizce seçilmez; **Needs review** olarak görünür. Aynı içeriğe sahip medya tek dosyada tutulabilir, fakat kaynak görünümleri ve tüm albüm ilişkileri korunur. Kaynaklar silinmez. Timestamps manifestte UTC olarak tutulur; dosyanın indirme tarihiyle karıştırılmaz.

Kontrollü olarak kesilmiş aktarımı aynı planla sürdür:

```sh
./archivebridge resume --plan plan.json --out fotograf-arsivi
```

Mevcut dosyalar hash ile kontrol edilir; bozuk veya farklı dosyanın üzerine yazılmaz. İlk MVP zorla süreç sonlandırma sonrası toparlanmayı henüz desteklediğini iddia etmez. Kaynak arşivlerini, planı ve metadata'yı özel tut. Doğrulama seçilen arşiv parçaları ve manifest içindir; tüm hesabın eksiksiz yedeği veya manifestin imzalı doğruluğu anlamına gelmez.
