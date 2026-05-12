package main

import (
    "context"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "path/filepath"
    "runtime"
    "runtime/debug"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"
)

var (
    maxConcurrent, _ = strconv.Atoi(getEnv("MAX_CONCURRENT", "4"))
    conversionTimeout, _ = strconv.Atoi(getEnv("CONVERSION_TIMEOUT", "120"))
    maxFileSize, _ = strconv.ParseInt(getEnv("MAX_FILE_SIZE", "104857600"), 10, 64)
)

func getEnv(key, defaultValue string) string {
    if value := os.Getenv(key); value != "" {
        return value
    }
    return defaultValue
}

const (
    port          = "5000"
    cleanupAge    = 1 * time.Hour
)

type Converter struct {
    semaphore chan struct{}
    wg        sync.WaitGroup
    mu        sync.RWMutex
    stats     ConversionStats
}

type ConversionStats struct {
    Total      int64
    Success    int64
    Failed     int64
    InProgress int64
}

func NewConverter(maxConcurrent int) *Converter {
    return &Converter{
        semaphore: make(chan struct{}, maxConcurrent),
        stats:     ConversionStats{},
    }
}

func (c *Converter) updateStats(success bool) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.stats.Total++
    if success {
        c.stats.Success++
    } else {
        c.stats.Failed++
    }
    c.stats.InProgress--
}

func (c *Converter) startConversion() {
    c.mu.Lock()
    c.stats.InProgress++
    c.mu.Unlock()
    c.semaphore <- struct{}{}
}

func (c *Converter) finishConversion() {
    <-c.semaphore
}

func (c *Converter) getStats() ConversionStats {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.stats
}

func init() {
    log.Println("PDF Converter initialized")
}

func main() {
    converter := NewConverter(maxConcurrent)

    http.HandleFunc("/convert", converter.handleConvert)
    http.HandleFunc("/convert-html", converter.handleConvertHtml)
    http.HandleFunc("/health", handleHealth)
    http.HandleFunc("/stats", converter.handleStats)
    http.HandleFunc("/convert-zip", converter.handleConvertZip)

    server := &http.Server{
        Addr: "0.0.0.0:" + port,
        ReadTimeout:  30 * time.Second,
        WriteTimeout: 5 * time.Minute,
        IdleTimeout:  120 * time.Second,
    }

    log.Printf("PDF Converter server starting on %s", server.Addr)
    log.Printf("Chrome path: %s", getChromePath())
    log.Printf("LibreOffice path: %s", getLibreOfficePath())
    log.Printf("Max concurrent conversions: %d", maxConcurrent)

    if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        log.Fatal("Server failed:", err)
    }
}

func (c *Converter) handleConvertZip(w http.ResponseWriter, r *http.Request) {
    log.Printf("Received ZIP conversion request from %s", r.RemoteAddr)

    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)

    if err := r.ParseMultipartForm(maxFileSize); err != nil {
        http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
        return
    }
    defer r.MultipartForm.RemoveAll()

    file, _, err := r.FormFile("file")
    if err != nil {
        http.Error(w, "Failed to get file: "+err.Error(), http.StatusBadRequest)
        return
    }
    defer file.Close()

    // Сохраняем zip во временный файл
    tmpZipFile, err := os.CreateTemp("", "converter_zip_*.zip")
    if err != nil {
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer os.Remove(tmpZipFile.Name())
    defer tmpZipFile.Close()

    if _, err := io.Copy(tmpZipFile, file); err != nil {
        http.Error(w, "Failed to save zip file", http.StatusInternalServerError)
        return
    }
    tmpZipFile.Close()

    // Распаковываем
    tmpDir, err := os.MkdirTemp("", "converter_zip_*")
    if err != nil {
        http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(tmpDir)

    cmd := exec.Command("unzip", "-o", tmpZipFile.Name(), "-d", tmpDir)
    if output, err := cmd.CombinedOutput(); err != nil {
        http.Error(w, "Failed to unzip: "+string(output), http.StatusBadRequest)
        return
    }

    // Ищем HTML файл
    htmlFiles, _ := filepath.Glob(filepath.Join(tmpDir, "*.html"))
    if len(htmlFiles) == 0 {
        htmlFiles, _ = filepath.Glob(filepath.Join(tmpDir, "**", "*.html"))
    }
    if len(htmlFiles) == 0 {
        http.Error(w, "HTML file not found in zip", http.StatusBadRequest)
        return
    }

    htmlFile := htmlFiles[0]

    htmlContent, err := os.ReadFile(htmlFile)
    if err == nil {
        printStyles := `<style>
            @page {
                size: A4;
                margin: 0;
                padding: 0;
            }
            @media print {
                html, body {
                    margin: 0;
                    padding: 0;
                    width: 100%;
                    height: 100%;
                }
            }
        </style>`
        
        htmlContent = []byte(strings.Replace(string(htmlContent), "</head>", printStyles+"</head>", 1))
        os.WriteFile(htmlFile, htmlContent, 0644)
    }
    
    // Создаем PDF
    pdfFile := filepath.Join(tmpDir, "output.pdf")
    
    c.startConversion()
    resultChan := make(chan error, 1)

    go func() {
        defer c.finishConversion()
        err := c.chromeToPdf(htmlFile, pdfFile)
        resultChan <- err
    }()

    select {
    case err := <-resultChan:
        if err != nil {
            c.updateStats(false)
            http.Error(w, "Conversion failed: "+err.Error(), http.StatusInternalServerError)
            return
        }

        pdfFileHandle, err := os.Open(pdfFile)
        if err != nil {
            c.updateStats(false)
            http.Error(w, "PDF file not created", http.StatusInternalServerError)
            return
        }
        defer pdfFileHandle.Close()

        c.updateStats(true)

        w.Header().Set("Content-Type", "application/pdf")
        w.Header().Set("Content-Disposition", "attachment; filename=\"output.pdf\"")
        io.Copy(w, pdfFileHandle)

        log.Printf("Successfully converted ZIP to PDF")

    case <-time.After(time.Duration(conversionTimeout) * time.Second):
        c.updateStats(false)
        http.Error(w, "Conversion timeout", http.StatusRequestTimeout)
    }
}

func (c *Converter) handleConvert(w http.ResponseWriter, r *http.Request) {
    log.Printf("Received conversion request from %s", r.RemoteAddr)

    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)

    if err := r.ParseMultipartForm(maxFileSize); err != nil {
        http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
        return
    }
    defer r.MultipartForm.RemoveAll()

    file, header, err := r.FormFile("file")
    if err != nil {
        http.Error(w, "Failed to get file: "+err.Error(), http.StatusBadRequest)
        return
    }
    defer file.Close()

    ext := strings.ToLower(filepath.Ext(header.Filename))
    if !isSupportedFormat(ext) {
        http.Error(w, "Unsupported file format: "+ext, http.StatusBadRequest)
        return
    }

    // Создаем временный файл в /tmp
    tmpFile, err := os.CreateTemp("", "converter_*"+ext)
    if err != nil {
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer os.Remove(tmpFile.Name())
    defer tmpFile.Close()

    if _, err := io.Copy(tmpFile, file); err != nil {
        http.Error(w, "Failed to save temp file", http.StatusInternalServerError)
        return
    }
    tmpFile.Close()

    // Создаем временную директорию для output
    tmpDir, err := os.MkdirTemp("", "converter_output_*")
    if err != nil {
        http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(tmpDir)

    outputFilename := strings.TrimSuffix(header.Filename, ext) + ".pdf"

    c.startConversion()
    resultChan := make(chan error, 1)

    go func() {
        defer c.finishConversion()
        err := c.convertWithRetry(tmpFile.Name(), tmpDir, filepath.Join(tmpDir, outputFilename), 3)
        resultChan <- err
    }()

    select {
    case err := <-resultChan:
        if err != nil {
            c.updateStats(false)
            http.Error(w, "Conversion failed: "+err.Error(), http.StatusInternalServerError)
            return
        }

        outputPath := filepath.Join(tmpDir, outputFilename)
        
        pdfFile, err := os.Open(outputPath)
        if err != nil {
            c.updateStats(false)
            http.Error(w, "PDF file not created", http.StatusInternalServerError)
            return
        }
        defer pdfFile.Close()

        c.updateStats(true)

        w.Header().Set("Content-Type", "application/pdf")
        w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", outputFilename))
        
        io.Copy(w, pdfFile)

        log.Printf("Successfully converted: %s -> %s", header.Filename, outputFilename)

    case <-time.After(time.Duration(conversionTimeout) * time.Second):
        c.updateStats(false)
        http.Error(w, "Conversion timeout", http.StatusRequestTimeout)
    }
}

func (c *Converter) convertWithRetry(inputPath, outputDir, pdfPath string, maxAttempts int) error {
    var lastError error

    for attempt := 1; attempt <= maxAttempts; attempt++ {
        
        ctx, cancel := context.WithTimeout(context.Background(), time.Duration(conversionTimeout)*time.Second)

        cmd := exec.CommandContext(ctx, "soffice",
            "--headless",
            "--norestore",
            "--convert-to", "pdf",
            "--outdir", outputDir,
            inputPath,
        )
        
        cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
        cmd.Env = append(os.Environ(), "OOO_DISABLE_PDF_SIGNATURE=1")

        output, err := cmd.CombinedOutput()
        log.Printf("LibreOffice output: %s", string(output))
        cancel()

        if _, err := os.Stat(pdfPath); err == nil {
            runtime.GC()
            debug.FreeOSMemory()
            return nil
        }
        
        files, _ := filepath.Glob(filepath.Join(outputDir, "*.pdf"))
        if len(files) > 0 {
            os.Rename(files[0], pdfPath)
            if _, err := os.Stat(pdfPath); err == nil {
                runtime.GC()
                debug.FreeOSMemory()
                return nil
            }
        }

        if ctx.Err() == context.DeadlineExceeded || err != nil {
            if attempt < maxAttempts {
                lastError = fmt.Errorf("attempt %d failed: %v", attempt, err)
                time.Sleep(time.Duration(attempt) * time.Second)
                continue
            }
            lastError = fmt.Errorf("all attempts failed: %v", err)
        }
    }

    return lastError
}

func (c *Converter) handleConvertHtml(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)

    var htmlContent string

    if r.MultipartForm != nil {
        file, header, err := r.FormFile("html")
        if err == nil {
            defer file.Close()
            content, _ := io.ReadAll(file)
            htmlContent = string(content)
            log.Printf("Received HTML file: %s, size: %d", header.Filename, len(htmlContent))
        }
    }

    if htmlContent == "" {
        htmlContent = r.FormValue("html")
    }

    if htmlContent == "" {
        http.Error(w, "No HTML content provided", http.StatusBadRequest)
        return
    }

    if len(htmlContent) > 5*1024*1024 {
        http.Error(w, "HTML too large (max 5MB)", http.StatusBadRequest)
        return
    }

    // Создаем временный HTML файл
    tmpHtmlFile, err := os.CreateTemp("", "converter_html_*.html")
    if err != nil {
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer os.Remove(tmpHtmlFile.Name())

    if _, err := tmpHtmlFile.Write([]byte(htmlContent)); err != nil {
        http.Error(w, "Failed to write HTML file", http.StatusInternalServerError)
        return
    }
    tmpHtmlFile.Close()

    // Создаем временную директорию для PDF
    tmpDir, err := os.MkdirTemp("", "converter_html_output_*")
    if err != nil {
        http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(tmpDir)

    pdfFile := filepath.Join(tmpDir, fmt.Sprintf("document_%d.pdf", time.Now().UnixNano()))

    c.startConversion()
    resultChan := make(chan error, 1)

    go func() {
        defer c.finishConversion()
        err := c.chromeToPdf(tmpHtmlFile.Name(), pdfFile)
        resultChan <- err
    }()

    select {
    case err := <-resultChan:
        if err != nil {
            c.updateStats(false)
            http.Error(w, "Conversion failed: "+err.Error(), http.StatusInternalServerError)
            return
        }

        pdfFileHandle, err := os.Open(pdfFile)
        if err != nil {
            c.updateStats(false)
            http.Error(w, "PDF file not created", http.StatusInternalServerError)
            return
        }
        defer pdfFileHandle.Close()

        info, _ := pdfFileHandle.Stat()
        if info.Size() == 0 {
            c.updateStats(false)
            http.Error(w, "PDF file is empty", http.StatusInternalServerError)
            return
        }

        c.updateStats(true)

        w.Header().Set("Content-Type", "application/pdf")
        w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(pdfFile)))
        
        io.Copy(w, pdfFileHandle)

        log.Printf("Successfully converted HTML to PDF: %s", pdfFile)

    case <-time.After(time.Duration(conversionTimeout) * time.Second):
        c.updateStats(false)
        http.Error(w, "Conversion timeout", http.StatusRequestTimeout)
    }
}

func (c *Converter) chromeToPdf(htmlFile, pdfFile string) error {
    chromePath := getChromePath()
    if chromePath == "" {
        return fmt.Errorf("Chrome/Chromium not found")
    }

    log.Printf("Using Chrome: %s", chromePath)

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    args := []string{
        "--headless=new",
        "--disable-gpu",
        "--no-sandbox",
        "--disable-crashpad-for-testing",
        "--disable-setuid-sandbox",
        "--disable-dev-shm-usage",
        "--disable-web-security",
        "--disable-features=IsolateOrigins,site-per-process",
        "--disable-extensions",
        "--disable-background-networking",
        "--disable-sync",
        "--no-first-run",
        "--disable-default-apps",
        "--disable-hang-monitor",
        "--print-to-pdf=" + pdfFile,
        "--print-to-pdf-no-header",
        "--no-pdf-header-footer",
        "--margin-top=0",
        "--margin-bottom=0",
        "--margin-left=0",
        "--margin-right=0",
        "--paper-width=210mm",
        "--paper-height=297mm",
        "--no-margins",
        "file://" + htmlFile,
    }

    cmd := exec.CommandContext(ctx, chromePath, args...)
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

    if err := cmd.Start(); err != nil {
        return fmt.Errorf("Chrome start error: %v", err)
    }

    done := make(chan error, 1)
    go func() {
        done <- cmd.Wait()
    }()

    select {
    case err := <-done:
        if err != nil {
            if _, statErr := os.Stat(pdfFile); statErr == nil {
                info, _ := os.Stat(pdfFile)
                if info.Size() > 0 {
                    log.Printf("Chrome conversion partial success, PDF size: %d bytes", info.Size())
                    return nil
                }
            }
            return fmt.Errorf("Chrome error: %v", err)
        }
    case <-ctx.Done():
        cmd.Process.Kill()
        if _, statErr := os.Stat(pdfFile); statErr == nil {
            info, _ := os.Stat(pdfFile)
            if info.Size() > 0 {
                log.Printf("Chrome timeout but PDF created, size: %d bytes", info.Size())
                return nil
            }
        }
        return fmt.Errorf("Chrome timeout after 30s")
    }

    info, _ := os.Stat(pdfFile)
    log.Printf("Chrome conversion successful, PDF size: %d bytes", info.Size())
    debug.FreeOSMemory()
    return nil
}

func getChromePath() string {
    paths := []string{
        "/usr/bin/chromium-browser",
        "/usr/bin/chromium",
        "/usr/bin/google-chrome-stable",
    }

    for _, path := range paths {
        if _, err := os.Stat(path); err == nil {
            return path
        }
    }

    return ""
}

func (c *Converter) handleStats(w http.ResponseWriter, r *http.Request) {
    stats := c.getStats()
    response := fmt.Sprintf(`{
        "total": %d,
        "success": %d,
        "failed": %d,
        "in_progress": %d,
        "queue_length": %d
    }`, stats.Total, stats.Success, stats.Failed, stats.InProgress, len(c.semaphore))

    w.Header().Set("Content-Type", "application/json")
    w.Write([]byte(response))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
}

func getLibreOfficePath() string {
    path, err := exec.LookPath("soffice")
    if err != nil {
        return "not found"
    }
    return path
}

func isSupportedFormat(ext string) bool {
    supported := map[string]bool{
        ".doc": true, ".docx": true, ".odt": true, ".rtf": true, ".txt": true,
        ".xls": true, ".xlsx": true, ".ods": true, ".csv": true,
        ".ppt": true, ".pptx": true, ".odp": true,
        ".html": true, ".htm": true, ".xml": true,
        ".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".bmp": true,
        ".tiff": true, ".tif": true,
    }
    return supported[ext]
}