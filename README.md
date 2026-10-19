# rawtojpg

## Konversi melalui GitHub Actions

Push file RAW yang didukung ke repository untuk menjalankan workflow konversi secara otomatis. Anda juga dapat menjalankannya dari tab **Actions** dengan memilih **Convert RAW images** lalu **Run workflow**. Hasil JPG tersedia sebagai artifact bernama `converted-jpg` pada halaman run tersebut.

Workflow memakai Git LFS saat checkout. File di atas 100 MB perlu disimpan dengan Git LFS, dan penggunaan LFS tetap tunduk pada kuota akun GitHub. GitHub Actions menjalankan konversi dan menyediakan hasil unduhan; workflow ini tidak meng-host aplikasi web Go.