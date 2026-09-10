# Personal Cloud Storage - Design Document

**Project:** Photo & Video Cloud Storage (Like Google Drive)
**Storage:** Backblaze B2 (primary) + VPS cache (optional)
**Date:** 2026-09-10
**Status:** Design Phase

---

## 📋 PROJECT OVERVIEW

### **Goal**
Build personal cloud storage for photos/videos with:
- Unlimited storage (B2 backend)
- Fast streaming (B2 CDN)
- Simple UI (like Google Drive)
- Disaster-proof (automatic backup)
- $0.40/month cost

### **Non-Goals**
- Team collaboration (single user)
- Complex permissions
- Office docs (media only)
- Social features

---

## 🏗️ ARCHITECTURE

### **Technology Stack**

**Backend:**
- Language: Go (high performance, single binary)
- Framework: Gin (lightweight REST API)
- Database: SQLite (metadata only)
- Storage: Backblaze B2 (object storage)
- Caching: Redis (optional, for frequent access)

**Frontend:**
- Framework: React or Vue.js
- UI Library: TailwindCSS
- Upload: Dropzone.js
- Media player: Video.js

**Infrastructure:**
- Hosting: VPS (existing 43.157.230.218)
- Domain: cloud.faidz.fun (subdomain)
- SSL: Let's Encrypt (existing setup)
- Reverse proxy: Nginx (existing)

---

## 📊 DATA MODEL

### **Database Schema (SQLite)**

```sql
-- Files table
CREATE TABLE files (
  id INTEGER PRIMARY KEY,
  user_id INTEGER,
  filename TEXT NOT NULL,
  file_type TEXT, -- 'image', 'video', 'document'
  mime_type TEXT, -- 'image/jpeg', 'video/mp4'
  file_size INTEGER, -- bytes
  b2_file_id TEXT, -- Backblaze B2 file ID
  b2_url TEXT, -- Direct B2 download URL
  thumbnail_url TEXT, -- B2 URL to thumbnail
  uploaded_at TIMESTAMP,
  created_at TIMESTAMP,
  updated_at TIMESTAMP,
  is_deleted BOOLEAN DEFAULT 0,
  folder_id INTEGER, -- For folder structure
  metadata JSON -- EXIF, duration, resolution, etc
);

-- Folders table
CREATE TABLE folders (
  id INTEGER PRIMARY KEY,
  user_id INTEGER,
  folder_name TEXT NOT NULL,
  parent_folder_id INTEGER,
  created_at TIMESTAMP,
  updated_at TIMESTAMP
);

-- User table
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT,
  email TEXT,
  storage_used INTEGER DEFAULT 0, -- bytes
  storage_limit INTEGER, -- bytes
  created_at TIMESTAMP,
  last_login TIMESTAMP
);

-- Sharing table
CREATE TABLE shares (
  id INTEGER PRIMARY KEY,
  file_id INTEGER,
  share_token TEXT UNIQUE,
  share_type TEXT, -- 'view', 'download'
  expires_at TIMESTAMP,
  created_at TIMESTAMP
);
```

---

## 🔄 API ENDPOINTS

### **Authentication**
```
POST   /api/auth/register          - Create user
POST   /api/auth/login             - Login
POST   /api/auth/logout            - Logout
POST   /api/auth/refresh           - Refresh token
```

### **Files**
```
GET    /api/files                  - List files (with pagination)
GET    /api/files?folder_id=X      - List by folder
GET    /api/files/search?q=query   - Search files
POST   /api/files/upload           - Upload file
GET    /api/files/:id/download     - Download file (redirect to B2)
DELETE /api/files/:id              - Delete file
PATCH  /api/files/:id/rename       - Rename file
GET    /api/files/:id/info         - Get file metadata
```

### **Folders**
```
GET    /api/folders                - List folders
POST   /api/folders                - Create folder
DELETE /api/folders/:id            - Delete folder
PATCH  /api/folders/:id/rename     - Rename folder
```

### **Sharing**
```
POST   /api/shares                 - Create share link
GET    /api/shares/:token          - Access shared file
DELETE /api/shares/:id             - Revoke share
GET    /api/shares                 - List all shares
```

### **Storage**
```
GET    /api/storage/usage          - Get storage stats
GET    /api/storage/stats          - Stats (by type, size, etc)
```

### **Admin**
```
GET    /api/admin/health           - Health check
GET    /api/admin/logs             - System logs
```

---

## 🗂️ PROJECT STRUCTURE

```
cloud-storage/
├── backend/
│   ├── main.go
│   ├── config/
│   │   └── config.go
│   ├── api/
│   │   ├── auth.go
│   │   ├── files.go
│   │   ├── folders.go
│   │   └── shares.go
│   ├── service/
│   │   ├── file_service.go
│   │   ├── b2_service.go
│   │   ├── folder_service.go
│   │   └── auth_service.go
│   ├── db/
│   │   ├── db.go
│   │   └── migrations/
│   │       └── init.sql
│   ├── middleware/
│   │   ├── auth.go
│   │   └── error_handler.go
│   ├── models/
│   │   ├── user.go
│   │   ├── file.go
│   │   └── folder.go
│   ├── utils/
│   │   ├── jwt.go
│   │   ├── validators.go
│   │   └── helpers.go
│   ├── go.mod
│   ├── go.sum
│   └── Dockerfile
│
├── frontend/
│   ├── src/
│   │   ├── index.jsx
│   │   ├── App.jsx
│   │   ├── pages/
│   │   │   ├── Dashboard.jsx
│   │   │   ├── Login.jsx
│   │   │   └── FileDetail.jsx
│   │   ├── components/
│   │   │   ├── FileGrid.jsx
│   │   │   ├── UploadZone.jsx
│   │   │   ├── FolderTree.jsx
│   │   │   ├── FilePlayer.jsx
│   │   │   └── ShareModal.jsx
│   │   ├── api/
│   │   │   └── client.js
│   │   ├── hooks/
│   │   │   ├── useAuth.js
│   │   │   └── useFiles.js
│   │   ├── styles/
│   │   │   └── globals.css
│   │   └── utils/
│   │       ├── auth.js
│   │       └── formatters.js
│   ├── package.json
│   ├── vite.config.js
│   └── Dockerfile
│
├── docker-compose.yml
├── nginx.conf
├── .env.example
└── README.md
```

---

## 🔄 USER FLOW

### **Upload Flow**
```
1. User select file(s)
   ↓
2. Frontend: Show upload progress
   ↓
3. POST /api/files/upload
   - Validate file (type, size)
   - Generate thumbnail (async)
   ↓
4. Backend: Upload to B2
   - Get B2 upload URL
   - Stream file to B2
   ↓
5. Backend: Save metadata to SQLite
   - filename, B2 URL, thumbnail
   ↓
6. Return file info to frontend
   ↓
7. Update UI with new file
```

### **Download Flow**
```
1. User click download
   ↓
2. GET /api/files/:id/download
   ↓
3. Backend: Verify access
   ↓
4. Redirect to B2 URL
   ↓
5. B2 CDN serves file (fast)
```

### **View Flow**
```
1. User click file (image/video)
   ↓
2. Show preview with metadata
   - Image: Show inline
   - Video: Video.js player
   ↓
3. Load thumbnail from B2 (cached)
   ↓
4. Load full file on demand
```

---

## 💾 STORAGE STRATEGY

### **File Organization in B2**
```
b2://cloud-storage-bucket/
├── users/
│   └── {user_id}/
│       ├── originals/
│       │   ├── {file_id}.jpg
│       │   ├── {file_id}.mp4
│       │   └── {file_id}.png
│       ├── thumbnails/
│       │   ├── {file_id}_thumb.jpg
│       │   └── {file_id}_thumb.jpg
│       └── metadata/
│           └── {file_id}.json
```

### **Caching Strategy (VPS)**
```
Optional: Cache recently accessed files
- Keep last 30 days of access
- Max 5GB cache
- Auto-cleanup old files
- Check B2 for updates
```

---

## 🔐 SECURITY

### **Authentication**
- JWT tokens (expires 24h)
- Refresh tokens (expires 30d)
- Password hashing: bcrypt

### **File Access**
- Check ownership before download
- Share tokens with expiration
- No direct B2 access from client

### **Data Protection**
- HTTPS only (SSL)
- Input validation
- Rate limiting
- SQL injection prevention (parameterized queries)

### **B2 Setup**
- Private bucket (not public)
- API key with limited scope
- Automatic versioning (keep 1 version)

---

## 📈 PERFORMANCE TARGETS

| Metric | Target |
|--------|--------|
| Upload speed | 10MB/s (limited by VPS bandwidth) |
| Download speed | 50MB/s (B2 CDN) |
| Page load | <2s |
| Search | <500ms (10k files) |
| Thumbnail gen | <5s (background job) |
| Concurrent users | 100+ (B2 unlimited) |

---

## 💰 COST BREAKDOWN

| Item | Cost | Notes |
|------|------|-------|
| VPS (existing) | $5-10/mo | Already have |
| B2 Storage (50GB) | $0.30/mo | $0.006/GB |
| B2 Download | $0.10/mo | Estimated |
| B2 Upload API | $0.00 | Free |
| Domain | $0 | Existing |
| SSL | $0 | Let's Encrypt |
| **TOTAL** | **$0.40/mo** | Beyond VPS |

---

## 🚀 DEPLOYMENT

### **Phase 1: Development**
- Setup Go backend locally
- Setup React frontend
- Local SQLite
- Mock B2 integration

### **Phase 2: Testing**
- Connect real B2 bucket
- Upload/download tests
- Performance tests
- Security review

### **Phase 3: Production**
- Deploy backend to VPS
- Deploy frontend to VPS
- Setup Nginx reverse proxy
- SSL certificate
- Database migration
- Monitoring setup

### **Phase 4: Maintenance**
- Daily backup
- Log monitoring
- Performance metrics
- Security updates

---

## 📋 CHECKLIST

### **Backend**
- [ ] Setup Go project structure
- [ ] Connect to SQLite
- [ ] Implement JWT auth
- [ ] Integrate B2 API
- [ ] Create all endpoints
- [ ] Error handling
- [ ] Input validation
- [ ] Logging
- [ ] Rate limiting
- [ ] Unit tests

### **Frontend**
- [ ] Setup React project
- [ ] Login/register pages
- [ ] File grid view
- [ ] Upload zone
- [ ] File detail view
- [ ] Video player
- [ ] Image viewer
- [ ] Share modal
- [ ] Search
- [ ] Folder navigation

### **Infrastructure**
- [ ] B2 bucket setup
- [ ] API key created
- [ ] VPS domain setup
- [ ] Nginx config
- [ ] SSL certificate
- [ ] Database backup script
- [ ] Monitoring setup

---

## 📖 NEXT STEPS

1. **Approve design** - Confirm all decisions
2. **Setup environments** - Local, staging, production
3. **Start backend** - Go API implementation
4. **Start frontend** - React UI
5. **Integration** - Connect components
6. **Testing** - Full system test
7. **Deploy** - Production launch

---

## 🔗 DEPENDENCIES

**Go Libraries:**
- gin: REST framework
- gorm: Database ORM (optional, can use database/sql)
- b2: B2 API client
- jwt-go: JWT handling
- bcrypt: Password hashing

**Frontend:**
- React 18+
- Vite: Build tool
- TailwindCSS: Styling
- Dropzone: Upload
- Video.js: Video player

---

**Status:** Ready for development ✅

**Proceed to implementation?** 👇
