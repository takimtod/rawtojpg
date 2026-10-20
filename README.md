# rawtojpg

Aplikasi web untuk mengonversi file kamera RAW menjadi JPG. Jalankan server lokal dengan Go 1.22 atau lebih baru:

```sh
go run .
```

Buka `http://localhost:8080` di browser. File dikirim ke layanan iLoveIMG untuk diproses.

Browser mengunggah file dalam bagian 8 MiB agar file besar dapat melewati proxy dengan batas ukuran request per-request. Total file yang dipilih tetap dibatasi 200 MiB.
