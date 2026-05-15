# AWS WorkSpaces Applications Client: Installation & Audit Review

This document provides a technical review of the AWS WorkSpaces Applications client installation patterns on Windows, based on the WorkSpaces Applications install tutorial.

## Installation Paths Reference

| Component | Path | When it appears / Description |
| :--- | :--- | :--- |
| **Bootstrapper** | `C:\Program Files (x86)\Amazon WorkSpaces Applications Client Installer\AppStreamClient.exe` | **System-wide.** Placed by the Enterprise Deployment Tool MSI. This is the installer for the per-user client. |
| **Actual Client** | `%LocalAppData%\AppStreamClient\` (e.g., `C:\Users\<username>\AppData\Local\AppStreamClient\`) | **Per-user.** Installed the first time a user logs in after the Enterprise Deployment Tool (Bootstrapper) is installed. |
| **Versioned Subfolder** | `%LocalAppData%\AppStreamClient\app-<versionID>\` | Contains the actual binary and configuration files (e.g., `Log4Net.config`) for a specific version. |
| **Diagnostic Logs** | `C:\Users\<username>\AppData\Local\Amazon\AppStreamClient\` | **Per-user.** Created when the client runs and writes logs. Note the `Amazon` subfolder difference. |

## Critique & Observations

### 1. Naming Inconsistency ("The Identity Crisis")
There is a notable inconsistency between the product branding ("Amazon WorkSpaces") and the underlying technology ("AppStreamClient"). 
* The system-wide installer folder uses the brand name: `...\Amazon WorkSpaces Applications Client Installer\`.
* The executable inside and the per-user app data folders use the technology name: `AppStreamClient.exe` and `...\AppStreamClient\`.
* **Risk:** This can confuse administrators and automated audit tools that look for "WorkSpaces" and might miss "AppStream" components.

### 2. Log Path Divergence
While the application data is stored in `...\Local\AppStreamClient\`, the logs are stored in `...\Local\Amazon\AppStreamClient\`. 
* **Critique:** Splitting the base path between `Local\Amazon` and `Local\` for related data is a poor convention. It forces troubleshooters to look in two different places in the user's profile for a single application's footprint.

### 3. Two-Stage "Ghost" Deployment
The deployment follows a "Bootstrapper" pattern:
1. The MSI (system-wide) only installs the *installer*.
2. The *actual application* is only installed when a user logs in.
* **Audit Implication:** Verifying that the MSI is installed on a machine is **not** proof that the user has the client. An audit must check the user's `%LocalAppData%` to confirm the application was successfully provisioned for that specific user.

### 4. Versioned Path Bloat
The use of `app-<versionID>` subfolders suggests that updates might leave behind old versions if a robust cleanup mechanism is not in place. 
* **Troubleshooting Tip:** When checking `Log4Net.config`, ensure you are looking in the *active* version's subfolder.

## Audit & Troubleshooting Checklist

When auditing a machine for AWS WorkSpaces Applications health:

- [ ] **Check Bootstrapper:** Verify `C:\Program Files (x86)\Amazon WorkSpaces Applications Client Installer\AppStreamClient.exe` exists.
- [ ] **Check User Provisioning:** Verify `%LocalAppData%\AppStreamClient\app-<versionID>\` exists for the target user.
- [ ] **Verify Logs:** Look in `C:\Users\<username>\AppData\Local\Amazon\AppStreamClient\` for recent log activity if the client fails to start.
- [ ] **Configuration Check:** Inspect `%LocalAppData%\AppStreamClient\app-<versionID>\Log4Net.config` for logging levels.

## Relation to Plumb

Plumb is designed for semantic codebase navigation and modification. While Plumb currently prioritises POSIX environments (macOS/Linux), this review highlights the complexity of Windows-specific pathing that Plumb would need to handle for full Windows support:

1. **Environment Variable Expansion:** Plumb would need to natively resolve `%LocalAppData%` and other Windows-specific variables.
2. **Naming Semantics:** Discovery tools like `find_files` or `search_in_files` need to be aware of these naming discrepancies (WorkSpaces vs. AppStream) when investigating client issues.
3. **Atomic Writes:** As noted in `docs/todo.md`, Plumb's `safeWrite` relies on POSIX rename semantics. Supporting Windows would require an alternative atomic write path to handle these configuration files safely.

### 4. Plumb MCP — how it could be more helpful in this context

Plumb has been doing the heavy lifting for code I/O. What would have made this conversation faster and more reliable:

*   **A way to capture and display file diffs after edits.** Right now after each `edit_file` / `write_file` I have to re-read the whole file to verify, which burns tokens and risks me missing collateral damage. A built-in "show me the unified diff for the last edit" would let me self-check at a glance and would also let you see exactly what I changed without scrolling through reviews.
*   **Tail/follow semantics for logs.** When you sent the v1 install log, I could only read the static snapshot. If Plumb exposed a `tail -f`-style view of `C:\ProgramData\plumb\...` (via a small helper running on the Windows test device), I could watch the install live and tell you which line is failing in near-real-time. Right now we round-trip through "you copy log → upload to Claude → I read." A direct Plumb→remote-Windows bridge would be magic.
*   **Real shell execution on the Mac for Build.ps1 validation.** The bash_tool in my sandbox is network-disabled and isolated from your Mac. If Plumb exposed `run_command` against the workspace, I could syntax-check the PowerShell, run a unit test against the `.reg` parser function, or invoke `pwsh -NoExecute -File Install-AppStreamMSIClient.ps1` to lint for parse errors before you have to upload to Intune and wait for the IMEx push. That alone would have caught the `reg.exe` import issue before pilot 1 because I could have actually run the script.
*   **Workspace-scoped git status / git diff.** I can see the repo tree but I can't tell which files have uncommitted changes since the last time you worked on something. A `plumb:workspace_diff` against HEAD would let me know what state we're really in — particularly useful when handing off to Claude Code, since I could pre-commit my edits or at least show you a clear summary of changed files.
*   **Symbol search with regex/glob excludes.** `search_in_files` is solid but the client folder kept showing up in results for "things I should ignore". A `--exclude-glob='appstream/**'` or workspace-level "active folder" hint would have saved several false leads.
*   **A way to attach context to Plumb actions for your dashboard.** You mentioned earlier that the Plumb dashboard didn't show my session. If `session_start` accepted something like `purpose="intune-project-deployment"` and tagged every subsequent action, your dashboard could show coherent timelines per task — and let you audit what I did across multi-turn conversations without trawling the Claude.ai history.

The first three would be the highest-impact for incident-investigation tasks like this one — diff visibility, live log tailing, and the ability to actually run the code I'm writing.
