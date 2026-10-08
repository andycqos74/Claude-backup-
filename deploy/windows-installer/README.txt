Central Backup Agent - Windows installer
=========================================

TO INSTALL
  1. Copy this whole folder onto the PC (keep the files together).
  2. Double-click   Install.cmd
  3. Click "Yes" when Windows asks for administrator permission.
  4. If you are asked, paste the Server URL, enrollment token and
     fingerprint from the backup server's Clients page.
     (Installers downloaded straight from the server already contain
     these details, so you will not be asked for them.)

That's it. The agent runs as a Windows service (CentralBackupAgent) and
appears under Clients in the server GUI within a few seconds.

TO UNINSTALL
  Double-click   Uninstall.cmd   and approve the administrator prompt.

NOTES
  * Use the x64 package on 64-bit Windows (almost all modern PCs); use the
    x86 package only on 32-bit Windows.
  * The agent makes only outbound HTTPS connections to the server and opens
    no inbound ports.
