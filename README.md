# Media Manipulator API

A Go backend service for converting images, videos, and audio files. Built to work seamlessly with the frontend UI counterpart. Uses powerful CLI conversion tools such as `ffmpeg` and `magick` (ImageMagick) to perform async conversions, and then the API stores the finished file and tracks job progress.

## Features

### Image Conversion
- **Formats**: JPG, PNG, WebP, GIF
- **Transformations**: Resize (width/height), quality adjustment
- **Filters**: Grayscale, sepia, blur, sharpen, swirl, barrel-distortion, oil-painting, vintage, emboss, charcoal, sketch, rotate-45º, rotate-90º, rotate-180º, rotate-270º
- **Color**: Tint application

### Video Conversion
- **Formats**: MP4, WebM, AVI, MOV
- **Transformations**: Resize with aspect ratio preservation
- **Effects**: Speed adjustment, quality levels (low/medium/high)
- **Optimization**: Efficient encoding with progress tracking

### Audio Conversion
- **Formats**: MP3, WAV, AAC, OGG
- **Settings**: Bitrate control (128-320 kbps)
- **Effects**: Speed and volume adjustment

## Architecture

```
media_manipulator_api/
├── cmd/api/main.go              # Application entry point
├── internal/
│   ├── config/config.go         # Configuration management
│   ├── models/conversion.go     # Data models and types
│   ├── handlers/conversion.go   # HTTP request handlers
│   ├── services/
│   │   ├── converter.go         # Core conversion logic
│   │   └── job_manager.go       # Job tracking and management
│   └── storage/local.go         # File storage utilities
├── uploads/                     # Temporary uploaded files
├── outputs/                     # Converted output files
├── Dockerfile                   # Container configuration
├── docker-compose.yml           # Development setup
└── go.mod                       # Go dependencies
```

## Prerequisites

### System Requirements
- Go 1.21 or later
- FFmpeg (for video/audio conversion)
- ImageMagick (optional, for advanced image processing)

### Installing FFmpeg

**Ubuntu/Debian:**
```bash
sudo apt update && sudo apt install ffmpeg
```

**macOS:**
```bash
brew install ffmpeg
```

**Windows:**
- Download from [https://ffmpeg.org/download.html](https://ffmpeg.org/download.html)
- Add to system PATH

## Quick Start

### 1. Clone and Setup
```bash
git clone <repository-url>
cd file-converter-backend
go mod tidy
```

### 2. Create Required Directories
```bash
mkdir -p uploads outputs
```

### 3. Run the Server
```bash
go run cmd/api/main.go
```

The server will start on `http://localhost:8080`

### 4. Test the API
```bash
# Health check
curl http://localhost:8080/api/health

# Should return: {"status":"healthy"}
```

## Docker Deployment

### Using Docker Compose (Recommended)
```bash
docker-compose up -d
```

### Using Docker directly
```bash
# Build image
docker build -t file-converter-backend .

# Run container
docker run -p 8080:8080 -v $(pwd)/uploads:/app/uploads -v $(pwd)/outputs:/app/outputs file-converter-backend
```

## API Endpoints

### POST /api/details
Analyze a file and get the details.

**Request:**
- Content-Type: `multipart/form-data`
- Form fields:
  - `file`: The file to convert
  - `options`: JSON string with conversion options

**Response:**
```json
{
  "fileName": "example.png",
  "fileSize": "1.4MB",
  "fileType": "image",
  "mimeType": "image/png",
  "details": {...details},
  "tool": "ImageMagick identify",
  "rawOutput": "...output"
}
```

### POST /api/upload
Upload a file and start conversion process.

**Request:**
- Content-Type: `multipart/form-data`
- Form fields:
  - `file`: The file to convert
  - `options`: JSON string with conversion options

**Response:**
```json
{
  "jobId": "abc123-def456-ghi789"
}
```

**Example options for image conversion:**
```json
{
  "format": "png",
  "width": 800,
  "height": 600,
  "quality": 85,
  "filter": "none"
}
```

### GET /api/job/:jobId
Check the status of a conversion job.

**Response:**
```json
{
  "id": "abc123-def456-ghi789",
  "status": "completed",
  "progress": 100,
  "resultUrl": "/api/download/abc123-def456-ghi789",
  "originalFile": {
    "name": "image.jpg",
    "size": 1024000,
    "type": "image/jpeg"
  },
  "createdAt": "2024-01-15T10:30:00Z",
  "completedAt": "2024-01-15T10:30:45Z"
}
```

**Status values:**
- `pending`: Job created, waiting to start
- `processing`: Conversion in progress
- `completed`: Conversion finished successfully
- `failed`: Conversion failed

### GET /api/download/:jobId
Download the converted file.

**Response:** Binary file with appropriate headers

## Configuration

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `UPLOAD_DIR` | `uploads` | Directory for uploaded files |
| `OUTPUT_DIR` | `outputs` | Directory for converted files |

## Frontend Integration

This backend is designed to work with the provided React frontend. The API matches the expected interface:

```typescript
// Frontend API calls match these endpoints
const uploadFile = async (file: File, options: ConversionFormData) => {
  const formData = new FormData();
  formData.append('file', file);
  formData.append('options', JSON.stringify(options));

  const response = await fetch('/api/upload', {
    method: 'POST',
    body: formData,
  });

  return response.json();
};

const checkJobStatus = async (jobId: string) => {
  const response = await fetch(`/api/job/${jobId}`);
  return response.json();
};
```

## Development

### Running in Development Mode
```bash
# Install air for hot reloading (optional)
go install github.com/cosmtrek/air@latest

# Run with hot reload
air

# Or run directly
go run cmd/api/main.go
```

### Project Structure Explained

- **cmd/api/**: Application entry points
- **internal/**: Private application code
  - **models/**: Data structures and types
  - **handlers/**: HTTP request handlers
  - **services/**: Business logic and processing
  - **config/**: Configuration management
- **uploads/**: Temporary storage for uploaded files
- **outputs/**: Storage for converted files

### Adding New Conversion Types

1. Add new options struct in `internal/models/conversion.go`
2. Implement conversion logic in `internal/services/converter.go`
3. Update handlers to support the new type

## Performance Considerations

- **Concurrent Processing**: Jobs are processed asynchronously
- **Memory Management**: Large files are streamed, not loaded entirely into memory
- **Progress Tracking**: Real-time progress updates for long-running conversions
- **Cleanup**: Automatic cleanup of temporary files after processing

## Error Handling

The backend provides comprehensive error handling:

- **File validation**: Type and size checking
- **Conversion errors**: Detailed error messages for debugging
- **Resource management**: Proper cleanup of temporary files
- **Graceful degradation**: Partial success handling where possible

## Security Features

- **File type validation**: Only supported file types are processed
- **Size limits**: Configurable maximum file sizes
- **Path traversal protection**: Secure file handling
- **CORS configuration**: Properly configured for frontend integration

## Monitoring and Logging

- **Health endpoint**: `/api/health` for monitoring
- **Structured logging**: JSON formatted logs for production
- **Job tracking**: Complete audit trail for all conversions
- **Progress monitoring**: Real-time progress updates

## Troubleshooting

### Common Issues

1. **FFmpeg not found**
   ```
   Error: exec: "ffmpeg": executable file not found in $PATH
   ```
   Solution: Install FFmpeg and ensure it's in your PATH

2. **Permission denied on uploads/outputs**
   ```bash
   sudo chown -R $USER:$USER uploads outputs
   chmod 755 uploads outputs
   ```

3. **Port already in use**
   ```bash
   export PORT=8081
   go run cmd/api/main.go
   ```

### Debug Mode
Set environment variable for verbose logging:
```bash
export GIN_MODE=debug
go run cmd/api/main.go
```

## Contributing

1. Fork the repository
2. Create a feature branch: `git checkout -b feature-name`
3. Make your changes and test thoroughly
4. Submit a pull request with detailed description

## License

This project is licensed under the MIT License. See LICENSE file for details.
