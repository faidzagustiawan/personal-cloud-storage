# DEVELOPMENT PROMPT - Personal Cloud Storage (Photo & Video)

**Project:** Custom Photo/Video Cloud Storage
**Goal:** High-performance, minimal VPS resource, unlimited storage via B2
**Duration:** 3-4 weeks estimated
**Tech Stack:** Go + React + SQLite + Backblaze B2

---

## 📌 CORE REQUIREMENTS

### **Must Have (MVP)**
1. **Upload Files** - Photo & video only
   - Single/multiple file upload
   - Progress indication
   - Drag-and-drop support
   - Max file size: 500MB

2. **View Files** - Gallery + player
   - Image viewer (thumbnail + full)
   - Video player (H.264 support)
   - Metadata display (size, date, resolution)
   - Pagination (50 files/page)

3. **Organize** - Basic folder structure
   - Create/delete/rename folders
   - Move files between folders
   - No nested folders (v1)

4. **Authentication** - Simple login
   - Register user
   - Login/logout
   - JWT tokens (24h expiry)
   - Password hashing (bcrypt)

5. **Storage** - B2 integration
   - Upload to B2 automatically
   - Download from B2 (redirect)
   - Thumbnail generation
   - Metadata stored in SQLite

6. **Sharing** - Basic links
   - Generate public share link
   - View-only access
   - Expiring links (7 days default)

### **Nice to Have (v2)**
- Search by filename
- EXIF metadata display
- Bulk operations (delete, move)
- Folder sharing
- Download as ZIP
- Favorites/starred files
- Recently viewed
- Storage usage stats

### **Out of Scope (v3+)**
- Team sharing
- Comments
- AI search
- Face detection
- Video transcoding
- Mobile apps (use web PWA)

---

## 🏗️ TECH DECISIONS

### **Backend: Go**
**Why:**
- Single binary (easy deploy)
- Fast (similar to Rust, easier to write)
- Goroutines (handle concurrent uploads)
- Small memory footprint
- Standard library is solid

**Framework:** Gin (lightweight REST)
**Database:** SQLite (no separate service)
**Storage API:** Backblaze B2 SDK

### **Frontend: React**
**Why:**
- Component-based (reusable)
- Rich ecosystem
- Fast UI updates
- Good for media apps

**Build:** Vite (fast dev server)
**Styling:** TailwindCSS (utility-first)
**UI Components:** shadcn/ui or Headless UI

### **Infrastructure**
- VPS: Existing (43.157.230.218)
- Domain: cloud.faidz.fun (subdomain)
- Reverse proxy: Nginx (rewrite /cloud → :8080)
- SSL: Let's Encrypt (existing)
- Deployment: Systemd service + Docker (choice)

---

## 📐 DATABASE SCHEMA

```sql
-- Users
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  email TEXT,
  storage_used INTEGER DEFAULT 0,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Folders
CREATE TABLE folders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  folder_name TEXT NOT NULL,
  parent_folder_id INTEGER,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (user_id) REFERENCES users(id),
  UNIQUE(user_id, folder_name, parent_folder_id)
);

-- Files
CREATE TABLE files (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  folder_id INTEGER,
  filename TEXT NOT NULL,
  file_type TEXT, -- 'image' or 'video'
  mime_type TEXT,
  file_size INTEGER,
  b2_file_id TEXT UNIQUE,
  b2_url TEXT,
  thumbnail_b2_url TEXT,
  width INTEGER,
  height INTEGER,
  duration_seconds REAL,
  uploaded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  is_deleted BOOLEAN DEFAULT 0,
  deleted_at TIMESTAMP,
  FOREIGN KEY (user_id) REFERENCES users(id),
  FOREIGN KEY (folder_id) REFERENCES folders(id)
);

-- Shares
CREATE TABLE shares (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  file_id INTEGER NOT NULL,
  share_token TEXT UNIQUE NOT NULL,
  expires_at TIMESTAMP NOT NULL,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (user_id) REFERENCES users(id),
  FOREIGN KEY (file_id) REFERENCES files(id)
);

CREATE INDEX idx_files_user_id ON files(user_id);
CREATE INDEX idx_files_folder_id ON files(folder_id);
CREATE INDEX idx_files_deleted ON files(is_deleted);
CREATE INDEX idx_shares_token ON shares(share_token);
```

---

## 🔌 API ENDPOINTS

### **Auth**
```
POST   /api/auth/register           {username, password, email}
POST   /api/auth/login              {username, password}
POST   /api/auth/logout             (requires auth)
POST   /api/auth/refresh            (requires auth)
```

### **Files**
```
GET    /api/files                   ?folder_id=X&page=1&limit=50
POST   /api/files/upload            multipart/form-data
GET    /api/files/:id/info          (JSON metadata)
GET    /api/files/:id/download      (redirect to B2 URL)
DELETE /api/files/:id               (soft delete)
PATCH  /api/files/:id/rename        {new_filename}
GET    /api/files/search?q=query    (search by filename)
```

### **Folders**
```
GET    /api/folders                 (list all)
POST   /api/folders                 {folder_name, parent_id}
DELETE /api/folders/:id             (cascade delete files)
PATCH  /api/folders/:id/rename      {new_name}
```

### **Shares**
```
POST   /api/shares                  {file_id, expires_in_days}
GET    /api/shares/:token           (public, no auth needed)
DELETE /api/shares/:id              (requires auth)
```

### **Storage**
```
GET    /api/storage/usage           {used_bytes, total_bytes, usage_percent}
```

---

## 🎨 FRONTEND PAGES

### **1. Login Page**
- Username/password input
- Register link
- Remember me (optional)
- Error messages

### **2. Dashboard (Main)**
- File grid (thumbnail view)
- Folder breadcrumb navigation
- Sidebar (folder tree, storage info)
- Upload drop zone
- Search bar
- Sort/filter options

### **3. File Detail**
- Full image/video player
- Metadata (size, date, resolution, duration)
- Download button
- Share button (generate link)
- Delete button
- Back button

### **4. Settings**
- Logout
- Change password
- Storage info
- About

---

## 🚀 DEVELOPMENT PHASES

### **Phase 1: Backend Setup (Week 1)**
- [ ] Go project structure
- [ ] SQLite setup + migrations
- [ ] JWT authentication
- [ ] B2 API integration
- [ ] File upload endpoint
- [ ] Basic error handling
- [ ] API documentation (comments)

### **Phase 2: Backend Logic (Week 2)**
- [ ] Complete file endpoints
- [ ] Folder endpoints
- [ ] Share link generation
- [ ] Thumbnail generation (background job)
- [ ] Search functionality
- [ ] Rate limiting
- [ ] Logging

### **Phase 3: Frontend Setup (Week 1-2)**
- [ ] React project + Vite
- [ ] Login/register pages
- [ ] API client setup
- [ ] Authentication flow
- [ ] Main layout

### **Phase 4: Frontend Features (Week 2-3)**
- [ ] File grid view
- [ ] Upload component
- [ ] File detail view
- [ ] Image/video player
- [ ] Folder navigation
- [ ] Share modal
- [ ] Settings page

### **Phase 5: Integration (Week 3)**
- [ ] Connect frontend to backend
- [ ] Test all flows
- [ ] Error handling UI
- [ ] Loading states
- [ ] Responsive design

### **Phase 6: Deployment (Week 4)**
- [ ] B2 bucket setup
- [ ] VPS configuration
- [ ] Nginx reverse proxy
- [ ] SSL certificate
- [ ] Database backup script
- [ ] Monitoring setup

---

## 📋 IMPLEMENTATION CHECKLIST

### **Backend**
- [ ] Init Go project (go mod init)
- [ ] Gin server setup
- [ ] SQLite connection pooling
- [ ] User registration (hash password)
- [ ] User login (JWT generation)
- [ ] Auth middleware
- [ ] B2 bucket connection
- [ ] File upload handler
- [ ] File metadata storage
- [ ] Thumbnail generation
- [ ] File listing with pagination
- [ ] File deletion (soft delete)
- [ ] Folder CRUD
- [ ] Share link generation
- [ ] Public share access
- [ ] Search implementation
- [ ] Error handling
- [ ] Logging setup
- [ ] Rate limiting
- [ ] Unit tests (optional)

### **Frontend**
- [ ] React setup + Vite
- [ ] TailwindCSS setup
- [ ] API client (axios/fetch)
- [ ] Login form
- [ ] Register form
- [ ] Main dashboard layout
- [ ] File grid component
- [ ] Folder tree component
- [ ] Upload zone
- [ ] Image viewer
- [ ] Video player
- [ ] File detail modal
- [ ] Metadata display
- [ ] Share modal
- [ ] Settings page
- [ ] Search input
- [ ] Pagination
- [ ] Responsive design
- [ ] Error boundaries
- [ ] Loading states

### **Infrastructure**
- [ ] B2 bucket created
- [ ] B2 API credentials
- [ ] VPS directory setup
- [ ] Go binary build
- [ ] React build (npm run build)
- [ ] Systemd service file
- [ ] Nginx reverse proxy
- [ ] SSL certificate renewal
- [ ] Database backup cron
- [ ] Health check endpoint
- [ ] Monitoring/logging

---

## 🔒 SECURITY CHECKLIST

- [ ] Password hashing (bcrypt)
- [ ] JWT token signing
- [ ] HTTPS only
- [ ] CORS properly configured
- [ ] Input validation
- [ ] SQL injection prevention (parameterized queries)
- [ ] File type validation
- [ ] File size limits
- [ ] Rate limiting on upload
- [ ] B2 API key permissions (limited scope)
- [ ] User data isolation (WHERE user_id = ?)
- [ ] No sensitive data in logs
- [ ] Soft delete (data recovery)

---

## 💡 DEVELOPMENT TIPS

### **Backend**
- Use Gorm or sqlc for safer SQL
- Implement proper error responses (400, 401, 403, 500)
- Add request logging middleware
- Use goroutines for thumbnail generation
- Keep B2 credentials in env variables
- Add database migrations (sql-migrate or goose)

### **Frontend**
- Use context API for auth state
- Implement error boundary component
- Lazy load video/image players
- Use React Query for API calls
- Add skeleton loaders for better UX
- Test on mobile early

### **Deployment**
- Use Docker for consistency
- Keep database backups (daily to B2)
- Monitor disk usage
- Setup log rotation
- Test recovery procedures

---

## 📈 PERFORMANCE TARGETS

| Metric | Target | Method |
|--------|--------|--------|
| Upload | 10MB/s | Multi-threaded to B2 |
| Download | 50MB/s | B2 CDN |
| Page load | <2s | Code splitting, lazy load |
| List 1000 files | <500ms | Database indexing, pagination |
| Thumbnail gen | <5s/image | Background job, caching |

---

## 💾 DEPLOYMENT ARCHITECTURE

```
User Browser
    ↓
HTTPS (Cloudflare)
    ↓
Nginx Reverse Proxy (port 80/443)
    ├─ /cloud → Go API (localhost:8080)
    └─ /cloud/* → React static (Vite build)
    ↓
Go API (localhost:8080)
    ├─ SQLite (metadata)
    └─ B2 API (file storage)
```

---

## 🎯 SUCCESS CRITERIA

✅ Can upload photo/video
✅ Can view in gallery
✅ Can organize in folders
✅ Can share via link
✅ Can download files
✅ Responsive on mobile
✅ <300ms page load
✅ <500MB VPS RAM usage
✅ $0.50/month cost

---

## 📞 STUCK? ASK FOR:

When developing, ask for:
1. "Code review on [specific function]"
2. "Debug [error message]"
3. "Optimize [performance issue]"
4. "How to implement [feature]"
5. "Better error handling for [scenario]"

Provide:
- Error messages (exact)
- What you tried
- What you expected
- Code snippet (if relevant)

---

## 🚀 READY TO START?

Next steps:
1. Confirm tech stack (Go + React + SQLite + B2)
2. Create B2 bucket
3. Setup Go project structure
4. Start backend development

**Ready to begin?** 👇
