**To run the code without postgres and docker**

cd backend
go mod init backend 2>/dev/null || true
go mod tidy
export DB_DSN="skip"
export PORT=8080
go run .
