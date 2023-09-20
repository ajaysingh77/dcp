//go:build windows

package kubeconfig

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/microsoft/usvc-apiserver/pkg/process"
	"golang.org/x/sys/windows"
)

// Writes a kubeconfig file on Windows. If the process is running as an administrator, we want to ensure
// that the kubeconfig file is only readable by other elevated processes. This is to ensure that unelevated
// processes cannot connect to an elevated DCP to launch local processes. If the user is not running as admin,
// then we just write the file using default behavior.
func writeFile(path string, content []byte) error {
	// Get the actual token for the process
	var processToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &processToken); err != nil {
		return err
	}
	defer processToken.Close()

	// Get the SID for the Administrators group
	adminSid, err := process.GetBuiltInSid(windows.DOMAIN_ALIAS_RID_ADMINS)
	if err != nil {
		return err
	}
	defer func() {
		if err := windows.FreeSid(adminSid); err != nil {
			panic(fmt.Errorf("could not free sid: %w", err))
		}
	}()

	// Get a virtual token for the process (not the actual token) to determine if the user is an admin
	adminToken := windows.Token(0)
	isAdmin, err := adminToken.IsMember(adminSid)
	if err != nil {
		return err
	}

	// If the user is not running as an admin, just write the file using default behavior
	if !isAdmin {
		return os.WriteFile(path, content, 0600)
	}

	// Get the user who ran the process so we can get the SID
	tokenUser, err := processToken.GetTokenUser()
	if err != nil {
		return err
	}

	// Get the SID for the Local System account
	systemSid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}

	var explicitEntries []windows.EXPLICIT_ACCESS
	// Add an ACL entry for the user running the process
	explicitEntries = append(
		explicitEntries,
		windows.EXPLICIT_ACCESS{
			// Grant the user permission to read the ACL list for the file, read attributes, and delete the file
			// DO NOT grant read permission as we want to limit access to the file to elevated processes only
			AccessPermissions: windows.READ_CONTROL | windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(tokenUser.User.Sid),
			},
		},
	)

	// Add an ACL entry for the System account
	explicitEntries = append(
		explicitEntries,
		windows.EXPLICIT_ACCESS{
			// Grant the System SID standard permissions to the file
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSid),
			},
		},
	)

	// And an ACL entry for the Administrators group
	explicitEntries = append(
		explicitEntries,
		windows.EXPLICIT_ACCESS{
			// Grant the Administrators SID standard permissions to the file
			AccessPermissions: windows.STANDARD_RIGHTS_ALL | windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSid),
			},
		},
	)

	acl, err := windows.ACLFromEntries(explicitEntries, nil)
	if err != nil {
		return fmt.Errorf("could not create acl: %w", err)
	}

	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return fmt.Errorf("could not create security descriptor: %w", err)
	}

	if err := sd.SetDACL(acl, true, false); err != nil {
		return fmt.Errorf("could not set dacl: %w", err)
	}

	// Ensure that the Security Descriptor applies the ACL and does not inherit permissions from the parent directory
	if err := sd.SetControl(windows.SE_DACL_PRESENT, windows.SE_DACL_PRESENT); err != nil {
		return fmt.Errorf("could not set control flag: %w", err)
	}
	if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return fmt.Errorf("could not set control flag: %w", err)
	}
	if err := sd.SetControl(windows.SE_DACL_AUTO_INHERITED, 0); err != nil {
		return fmt.Errorf("could not set control flag: %w", err)
	}
	if err := sd.SetControl(windows.SE_DACL_AUTO_INHERIT_REQ, 0); err != nil {
		return fmt.Errorf("could not set control flag: %w", err)
	}

	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}

	pathHandle, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("could not create path handle: %w", err)
	}

	// Create the new file with the given ACL rules
	fileHandle, err := windows.CreateFile(pathHandle, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("could not create file: %w", err)
	}
	defer func() {
		if err := windows.CloseHandle(fileHandle); err != nil {
			panic(fmt.Errorf("could not close file handle: %w", err))
		}
	}()

	// Write the kubeconfig contents to the file
	_, err = windows.Write(fileHandle, content)
	if err != nil {
		return fmt.Errorf("could not write to file: %w", err)
	}

	return nil
}
