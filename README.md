# rawtojpg

Aplikasi web untuk mengonversi file kamera RAW menjadi JPG. Jalankan server lokal dengan Go 1.22 atau lebih baru:

```sh
go run .
```

Buka `http://localhost:8080` di browser. File dikirim ke layanan iLoveIMG untuk diproses.

## Deploy ke Vercel

Impor repository ini ke Vercel dan pastikan **Framework Preset** menggunakan **Go** serta **Root Directory** mengarah ke folder repository. Setelah mengubah konfigurasi, deploy ulang agar halaman dan endpoint `/api/convert` dijalankan oleh server Go.
