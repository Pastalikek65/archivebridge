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

Yayımlanmış 0.1.0 paketi, zarif iptalden sonra `resume` ile devam etmeyi destekler; mevcut dosyalar hash ile kontrol edilir ve bozuk ya da farklı dosyanın üzerine yazılmaz. `compare` ile ani süreç sonlandırması sonrası kurtarma, henüz yayımlanmamış 0.2 beta kaynak koduna aittir. Beta'yı denemek için Go 1.27.2 ile bu depoyu derle: Linux'ta `go build -trimpath -o archivebridge ./cmd/archivebridge`, Windows PowerShell'de `go build -trimpath -o archivebridge.exe ./cmd/archivebridge` kullan. Beta çapraz platform CI ve inceleme tamamlanana kadar nitelikli değildir. Kaynak arşivlerini, planı ve metadata'yı özel tut. Doğrulama seçilen arşiv parçaları ve manifest içindir; tüm hesabın eksiksiz yedeği veya manifestin imzalı doğruluğu anlamına gelmez.

`compare`, beta 0.2.0 derlemesinde planın işaret ettiği özgün arşivleri yeniden inceler ve planı manifest ile saklanan baytlarla karşılaştırır. Kaynak arşivleri özgün yollarında erişilebilir olmalıdır. Yayımlanmış 0.1.0 paketinde `compare` komutu bulunmaz.
