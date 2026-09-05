import ssh2 from "ssh2";
const { STATUS_CODE: S, OPEN_MODE: F } = ssh2.utils.sftp;
export function attachSFTP(session, files, control = {}) {
  session.on("sftp", (accept) => {
    const sftp = accept(),
      handles = new Map();
    let next = 0;
    sftp.on("end", () => sftp.end());
    const attrs = (file) => ({
      mode: file.mode || 0o100600,
      uid: 501,
      gid: 20,
      size: file.data.length,
      atime: 0,
      mtime: 0,
    });
    sftp.on("LSTAT", (id, path) =>
      files.has(path)
        ? sftp.attrs(id, attrs(files.get(path)))
        : sftp.status(id, S.NO_SUCH_FILE),
    );
    sftp.on("STAT", (id, path) =>
      files.has(path)
        ? sftp.attrs(id, attrs(files.get(path)))
        : sftp.status(id, S.NO_SUCH_FILE),
    );
    sftp.on("OPEN", (id, path, flags) => {
      if (files.has(path) && flags & F.EXCL) return sftp.status(id, S.FAILURE);
      if (!(flags & F.CREAT) && !files.has(path))
        return sftp.status(id, S.NO_SUCH_FILE);
      if (!files.has(path))
        files.set(path, { data: Buffer.alloc(0), mode: 0o100600 });
      const handle = Buffer.alloc(4);
      handle.writeUInt32BE(++next);
      handles.set(next, path);
      sftp.handle(id, handle);
    });
    sftp.on("WRITE", (id, handle, offset, data) => {
      const path = handles.get(handle.readUInt32BE()),
        file = files.get(path);
      if (!file) return sftp.status(id, S.FAILURE);
      const bytes = Buffer.alloc(
        Math.max(file.data.length, offset + data.length),
      );
      file.data.copy(bytes);
      data.copy(bytes, offset);
      file.data = bytes;
      if (control.delay)
        setTimeout(() => {
          if (!sftp.destroyed) sftp.status(id, S.OK);
        }, control.delay);
      else sftp.status(id, S.OK);
    });
    sftp.on("FSETSTAT", (id, handle, attributes) => {
      const file = files.get(handles.get(handle.readUInt32BE()));
      if (file) {
        if (attributes.mode) file.mode = attributes.mode;
        sftp.status(id, S.OK);
      } else sftp.status(id, S.FAILURE);
    });
    sftp.on("CLOSE", (id, handle) => {
      handles.delete(handle.readUInt32BE());
      sftp.status(id, S.OK);
    });
    sftp.on("RENAME", (id, oldPath, newPath) => {
      if (!files.has(oldPath) || files.has(newPath))
        return sftp.status(id, S.FAILURE);
      files.set(newPath, files.get(oldPath));
      files.delete(oldPath);
      sftp.status(id, S.OK);
    });
    sftp.on("REMOVE", (id, path) => {
      files.delete(path);
      sftp.status(id, S.OK);
    });
  });
}
