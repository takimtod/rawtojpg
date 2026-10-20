# rawtojpg

Aplikasi web untuk mengonversi file kamera RAW menjadi JPG. Jalankan server lokal dengan Go 1.22 atau lebih baru:

```sh
go run .
```

Buka `http://localhost:8080` di browser. File dikirim ke layanan iLoveIMG untuk diproses.

Browser mengunggah file dalam bagian 8 MiB agar file besar dapat melewati proxy dengan batas ukuran request per-request. Total file yang dipilih tetap dibatasi 200 MiB.

## Deploy ke Vercel

Vercel membatasi ukuran request ke Function, sehingga tidak cocok untuk endpoint ini yang menerima hingga 200 MiB. Gunakan server yang mengizinkan request sebesar itu, atau unggah file melalui layanan penyimpanan terpisah.

## Nginx reverse proxy

Jika aplikasi berjalan di belakang Nginx, naikkan batas ukuran request pada blok `server` yang meneruskan request ke aplikasi:

```nginx
client_max_body_size 10m;
```

Batas ini sedikit di atas ukuran bagian upload 8 MiB. Setelah menyimpan konfigurasi, validasi dan muat ulang Nginx:

```sh
sudo nginx -t
sudo systemctl reload nginx
```

Pastikan directive diterapkan pada virtual host yang benar. Jika aplikasi juga berjalan di platform proxy lain, batas request platform tersebut harus dinaikkan secara terpisah.