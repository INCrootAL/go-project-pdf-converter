package main

import (
    "context"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "os/signal"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"
    "mime/multipart"
)

var (
    maxConcurrent = getEnvInt("MAX_CONCURRENT", 4)
    conversionTimeout  = getEnvInt("CONVERSION_TIMEOUT", 120)
    maxFileSize = getEnvInt64("MAX_FILE_SIZE", 104857600) // 100MB
)

func getEnv(key, defaultValue string) string {
    if value := os.Getenv(key); value != "" {
        return value
    }
    return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
    if value := os.Getenv(key); value != "" {
        if i, err := strconv.Atoi(value); err == nil {
            return i
        }
    }
    return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
    if value := os.Getenv(key); value != "" {
        if i, err := strconv.ParseInt(value, 10, 64); err == nil {
            return i
        }
    }
    return defaultValue
}

const (
    port = "5000"
    maxHTMLSize = 5 * 1024 * 1024 // 5MB
    maxRetries  = 3
    chromeTimeout = 30 * time.Second
)

type Converter struct {
    semaphore chan struct{}
    mu        sync.RWMutex
    stats     ConversionStats
}

type ConversionStats struct {
    Total int64 `json:"total"`
    Success int64 `json:"success"`
    Failed int64 `json:"failed"`
    InProgress int64 `json:"in_progress"`
}

func NewConverter(maxConcurrent int) *Converter {
    return &Converter{
        semaphore: make(chan struct{}, maxConcurrent),
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
    c.semaphore <- struct{}{}
    c.mu.Lock()
    c.stats.InProgress++
    c.mu.Unlock()
}

func (c *Converter) finishConversion() {
    <-c.semaphore
}

func (c *Converter) getStats() ConversionStats {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.stats
}

func (c *Converter) handleConvert(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    file, header, err := parseUploadedFile(w, r)
    if err != nil {
        return // parseUploadedFile already writes error response
    }
    defer file.Close()

    ext := strings.ToLower(filepath.Ext(header.Filename))
    if !isSupportedFormat(ext) {
        http.Error(w, "Unsupported file format: "+ext, http.StatusBadRequest)
        return
    }

    // Save uploaded file to temp
    tmpFile, err := os.CreateTemp("", "converter_*"+ext)
    if err != nil {
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer os.Remove(tmpFile.Name())

    if _, err := io.Copy(tmpFile, file); err != nil {
        http.Error(w, "Failed to save file", http.StatusInternalServerError)
        return
    }
    tmpFile.Close()

    // Create output directory
    tmpDir, err := os.MkdirTemp("", "converter_output_*")
    if err != nil {
        http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(tmpDir)

    outputFilename := strings.TrimSuffix(header.Filename, ext) + ".pdf"
    outputPath := filepath.Join(tmpDir, outputFilename)

    // Convert
    pdfPath, err := c.executeConversion(r.Context(), func(ctx context.Context) (string, error) {
        return outputPath, c.convertWithRetry(ctx, tmpFile.Name(), tmpDir, outputPath, maxRetries)
    })

    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    c.sendFile(w, pdfPath, outputFilename)
}

func (c *Converter) handleConvertHtml(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    htmlContent, err := parseHTMLContent(w, r)
    if err != nil {
        return
    }

    // Save HTML to temp file
    tmpHtmlFile, err := os.CreateTemp("", "converter_html_*.html")
    if err != nil {
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer os.Remove(tmpHtmlFile.Name())

    if _, err := tmpHtmlFile.Write([]byte(htmlContent)); err != nil {
        http.Error(w, "Failed to write HTML", http.StatusInternalServerError)
        return
    }
    tmpHtmlFile.Close()

    // Create temp dir for PDF
    tmpDir, err := os.MkdirTemp("", "converter_html_output_*")
    if err != nil {
        http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(tmpDir)

    pdfFile := filepath.Join(tmpDir, fmt.Sprintf("document_%d.pdf", time.Now().UnixNano()))
    outputFilename := filepath.Base(pdfFile)

    // Convert
    pdfPath, err := c.executeConversion(r.Context(), func(ctx context.Context) (string, error) {
        return pdfFile, c.chromeToPdf(ctx, tmpHtmlFile.Name(), pdfFile)
    })

    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    c.sendFile(w, pdfPath, outputFilename)
}

func (c *Converter) handleConvertZip(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    file, _, err := parseUploadedFile(w, r)
    if err != nil {
        return
    }
    defer file.Close()

    // Save ZIP to temp
    tmpZipFile, err := os.CreateTemp("", "converter_zip_*.zip")
    if err != nil {
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer os.Remove(tmpZipFile.Name())

    if _, err := io.Copy(tmpZipFile, file); err != nil {
        http.Error(w, "Failed to save ZIP", http.StatusInternalServerError)
        return
    }
    tmpZipFile.Close()

    // Extract ZIP
    tmpDir, err := os.MkdirTemp("", "converter_zip_*")
    if err != nil {
        http.Error(w, "Failed to create temp dir", http.StatusInternalServerError)
        return
    }
    defer os.RemoveAll(tmpDir)

    cmd := exec.Command("unzip", "-o", tmpZipFile.Name(), "-d", tmpDir)
    if output, err := cmd.CombinedOutput(); err != nil {
        http.Error(w, fmt.Sprintf("Failed to unzip: %s", string(output)), http.StatusBadRequest)
        return
    }

    // Find HTML file
    htmlFile := findHTMLFile(tmpDir)
    if htmlFile == "" {
        http.Error(w, "HTML file not found in ZIP", http.StatusBadRequest)
        return
    }

    // Inject print styles
    if err := injectPrintStyles(htmlFile); err != nil {
        log.Printf("Warning: failed to inject print styles: %v", err)
    }

    // Convert
    pdfFile := filepath.Join(tmpDir, "output.pdf")
    outputFilename := "output.pdf"

    pdfPath, err := c.executeConversion(r.Context(), func(ctx context.Context) (string, error) {
        return pdfFile, c.chromeToPdf(ctx, htmlFile, pdfFile)
    })

    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }

    c.sendFile(w, pdfPath, outputFilename)
}

// executeConversion handles the common pattern: start conversion, run with timeout, handle stats
func (c *Converter) executeConversion(
    requestCtx context.Context,
    convertFn func(context.Context) (string, error),
) (string, error) {
    c.startConversion()

    ctx, cancel := context.WithTimeout(requestCtx, time.Duration(conversionTimeout)*time.Second)
    defer cancel()

    type result struct {
        path string
        err  error
    }

    resultChan := make(chan result, 1)

    go func() {
        defer c.finishConversion()
        pdfPath, err := convertFn(ctx)
        select {
        case resultChan <- result{pdfPath, err}:
        case <-ctx.Done():
            // Nobody
        }
    }()

    select {
    case res := <-resultChan:
        if res.err != nil {
            c.updateStats(false)
            return "", fmt.Errorf("conversion failed: %w", res.err)
        }
        c.updateStats(true)
        return res.path, nil
    case <-ctx.Done():
        c.updateStats(false)
        return "", fmt.Errorf("conversion timeout")
    }
}

func (c *Converter) convertWithRetry(ctx context.Context, inputPath, outputDir, pdfPath string, maxAttempts int) error {
    var lastError error

    for attempt := 1; attempt <= maxAttempts; attempt++ {
        libreHome, err := os.MkdirTemp("/tmp", "libre_home_*")
        if err != nil {
            return fmt.Errorf("failed to create libreoffice temp dir: %w", err)
        }

        attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(conversionTimeout)*time.Second)

        cmd := exec.CommandContext(attemptCtx, "soffice",
            "--headless",
            "--norestore",
            "--convert-to", "pdf",
            "--outdir", outputDir,
            inputPath,
        )

        cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
        cmd.Env = append(os.Environ(),
            "HOME="+libreHome,
            "OOO_DISABLE_PDF_SIGNATURE=1",
        )

        output, err := cmd.CombinedOutput()
        log.Printf("LibreOffice output: %s", string(output))

        cancel()
        os.RemoveAll(libreHome)

        // Check if PDF was created at expected path
        if info, statErr := os.Stat(pdfPath); statErr == nil && info.Size() > 0 {
            return nil
        }

        // LibreOffice might name the PDF differently
        files, _ := filepath.Glob(filepath.Join(outputDir, "*.pdf"))
        if len(files) > 0 {
            if err := os.Rename(files[0], pdfPath); err == nil {
                if info, statErr := os.Stat(pdfPath); statErr == nil && info.Size() > 0 {
                    return nil
                }
            }
        }

        if attemptCtx.Err() == context.DeadlineExceeded || err != nil {
            if attempt < maxAttempts {
                lastError = fmt.Errorf("attempt %d/%d failed: %v", attempt, maxAttempts, err)
                time.Sleep(time.Duration(attempt) * time.Second)
                continue
            }
            lastError = fmt.Errorf("all %d attempts failed, last error: %v", maxAttempts, err)
        }
    }

    return lastError
}

func (c *Converter) chromeToPdf(ctx context.Context, htmlFile, pdfFile string) error {
    chromeHome, err := os.MkdirTemp("/tmp", "chrome_home_*")
    if err != nil {
        return fmt.Errorf("failed to create chrome temp dir: %w", err)
    }
    defer os.RemoveAll(chromeHome)

    chromePath := getChromePath()
    if chromePath == "" {
        return fmt.Errorf("Chrome/Chromium not found")
    }

    log.Printf("Using Chrome: %s", chromePath)

    chromeCtx, cancel := context.WithTimeout(ctx, chromeTimeout)
    defer cancel()

    args := []string{
        "--headless=new",
        "--user-data-dir=" + filepath.Join(chromeHome, "user-data"),
        "--crash-dumps-dir=" + filepath.Join(chromeHome, "crashes"),
        "--disk-cache-dir=" + filepath.Join(chromeHome, "cache"),
        "--disable-gpu",
        "--no-sandbox",
        "--disable-dev-shm-usage",
        "--disable-web-security",
        "--disable-extensions",
        "--disable-background-networking",
        "--disable-sync",
        "--no-first-run",
        "--disable-default-apps",
        "--disable-hang-monitor",
        "--print-to-pdf=" + pdfFile,
        "--no-pdf-header-footer",
        "file://" + htmlFile,
    }

    cmd := exec.CommandContext(chromeCtx, chromePath, args...)
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

    cmd.Env = append(os.Environ(),
        "HOME="+chromeHome,
        "XDG_CACHE_HOME="+filepath.Join(chromeHome, ".cache"),
        "XDG_CONFIG_HOME="+filepath.Join(chromeHome, ".config"),
    )

    if err := cmd.Start(); err != nil {
        return fmt.Errorf("Chrome start error: %w", err)
    }

    done := make(chan error, 1)
    go func() {
        done <- cmd.Wait()
    }()

    select {
    case err := <-done:
        if err != nil {
            if info, statErr := os.Stat(pdfFile); statErr == nil && info.Size() > 0 {
                log.Printf("Chrome partial success, PDF size: %d bytes", info.Size())
                return nil
            }
            return fmt.Errorf("Chrome error: %w", err)
        }
    case <-chromeCtx.Done():
        cmd.Process.Kill()
        if info, statErr := os.Stat(pdfFile); statErr == nil && info.Size() > 0 {
            log.Printf("Chrome timeout but PDF created, size: %d bytes", info.Size())
            return nil
        }
        return fmt.Errorf("Chrome timeout after %v", chromeTimeout)
    }

    if info, err := os.Stat(pdfFile); err == nil {
        log.Printf("Chrome conversion successful, PDF size: %d bytes", info.Size())
    }
    return nil
}

func parseUploadedFile(w http.ResponseWriter, r *http.Request) (io.ReadCloser, *multipart.FileHeader, error) {
    r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)

    if err := r.ParseMultipartForm(maxFileSize); err != nil {
        http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
        return nil, nil, err
    }

    file, header, err := r.FormFile("file")
    if err != nil {
        http.Error(w, "Failed to get file: "+err.Error(), http.StatusBadRequest)
        return nil, nil, err
    }

    return file, header, nil
}

func parseHTMLContent(w http.ResponseWriter, r *http.Request) (string, error) {
    r.Body = http.MaxBytesReader(w, r.Body, maxHTMLSize)

    // Try multipart form first
    contentType := r.Header.Get("Content-Type")
    if strings.Contains(contentType, "multipart/form-data") {
        if err := r.ParseMultipartForm(maxHTMLSize); err != nil {
            http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
            return "", err
        }
        defer r.MultipartForm.RemoveAll()

        file, _, err := r.FormFile("html")
        if err == nil {
            defer file.Close()
            content, _ := io.ReadAll(file)
            return string(content), nil
        }
    }

    // Fall back to form value
    if err := r.ParseForm(); err != nil {
        http.Error(w, "Failed to parse form", http.StatusBadRequest)
        return "", err
    }

    htmlContent := r.FormValue("html")
    if htmlContent == "" {
        http.Error(w, "No HTML content provided", http.StatusBadRequest)
        return "", fmt.Errorf("no HTML content")
    }

    if len(htmlContent) > maxHTMLSize {
        http.Error(w, fmt.Sprintf("HTML too large (max %dMB)", maxHTMLSize/(1024*1024)), http.StatusBadRequest)
        return "", fmt.Errorf("HTML too large")
    }

    return htmlContent, nil
}

func (c *Converter) sendFile(w http.ResponseWriter, filePath, filename string) {
    file, err := os.Open(filePath)
    if err != nil {
        log.Printf("Failed to open PDF: %v", err)
        http.Error(w, "PDF file not found", http.StatusInternalServerError)
        return
    }
    defer file.Close()

    info, _ := file.Stat()
    if info.Size() == 0 {
        http.Error(w, "PDF file is empty", http.StatusInternalServerError)
        return
    }

    w.Header().Set("Content-Type", "application/pdf")
    w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

    if _, err := io.Copy(w, file); err != nil {
        log.Printf("Failed to send PDF: %v", err)
    }
}

func findHTMLFile(dir string) string {
    htmlFiles, _ := filepath.Glob(filepath.Join(dir, "*.html"))
    if len(htmlFiles) > 0 {
        return htmlFiles[0]
    }

    htmlFiles, _ = filepath.Glob(filepath.Join(dir, "*", "*.html"))
    if len(htmlFiles) > 0 {
        return htmlFiles[0]
    }

    return ""
}

func injectPrintStyles(htmlFile string) error {
    content, err := os.ReadFile(htmlFile)
    if err != nil {
        return err
    }

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

    newContent := strings.Replace(string(content), "</head>", printStyles+"</head>", 1)
    return os.WriteFile(htmlFile, []byte(newContent), 0644)
}

var chromePathCache string
var chromePathOnce sync.Once

func getChromePath() string {
    chromePathOnce.Do(func() {
        paths := []string{
            "/usr/bin/chromium-browser",
            "/usr/bin/chromium",
            "/usr/bin/google-chrome-stable",
        }
        for _, path := range paths {
            if _, err := os.Stat(path); err == nil {
                chromePathCache = path
                return
            }
        }
    })
    return chromePathCache
}

func getLibreOfficePath() string {
    path, err := exec.LookPath("soffice")
    if err != nil {
        return "not found"
    }
    return path
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
    w.Header().Set("Content-Type", "text/plain")
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("OK"))
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

func main() {
    log.Println("PDF Converter initialized")

    converter := NewConverter(maxConcurrent)

    mux := http.NewServeMux()
    mux.HandleFunc("/convert", converter.handleConvert)
    mux.HandleFunc("/convert-html", converter.handleConvertHtml)
    mux.HandleFunc("/convert-zip", converter.handleConvertZip)
    mux.HandleFunc("/health", handleHealth)
    mux.HandleFunc("/stats", converter.handleStats)

    server := &http.Server{
        Addr: "0.0.0.0:" + port,
        Handler: mux,
        ReadTimeout: 30 * time.Second,
        WriteTimeout: 5 * time.Minute,
        IdleTimeout: 120 * time.Second,
    }

    log.Printf("PDF Converter server starting on %s", server.Addr)
    log.Printf("Chrome path: %s", getChromePath())
    log.Printf("LibreOffice path: %s", getLibreOfficePath())
    log.Printf("Max concurrent conversions: %d", maxConcurrent)

    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    go func() {
        <-ctx.Done()
        log.Println("Shutting down server...")
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        if err := server.Shutdown(shutdownCtx); err != nil {
            log.Printf("Server shutdown error: %v", err)
        }
    }()

    if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        log.Fatal("Server failed:", err)
    }

    log.Println("Server stopped")
}