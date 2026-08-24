//go:build darwin && cgo

package auth

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

static CFMutableDictionaryRef jira_query(CFStringRef service, CFStringRef account) {
	CFMutableDictionaryRef query = CFDictionaryCreateMutable(
		kCFAllocatorDefault,
		0,
		&kCFTypeDictionaryKeyCallBacks,
		&kCFTypeDictionaryValueCallBacks
	);
	if (query == NULL) {
		return NULL;
	}
	CFDictionarySetValue(query, kSecClass, kSecClassGenericPassword);
	CFDictionarySetValue(query, kSecAttrService, service);
	CFDictionarySetValue(query, kSecAttrAccount, account);
	CFDictionarySetValue(query, kSecUseAuthenticationUI, kSecUseAuthenticationUIFail);
	return query;
}

static OSStatus jira_load(CFStringRef service, CFStringRef account, CFTypeRef *result) {
	CFMutableDictionaryRef query = jira_query(service, account);
	if (query == NULL) {
		return errSecAllocate;
	}
	CFDictionarySetValue(query, kSecMatchLimit, kSecMatchLimitOne);
	CFDictionarySetValue(query, kSecReturnData, kCFBooleanTrue);
	OSStatus status = SecItemCopyMatching(query, result);
	CFRelease(query);
	return status;
}

static OSStatus jira_add(CFStringRef service, CFStringRef account, CFDataRef value) {
	CFMutableDictionaryRef attributes = jira_query(service, account);
	if (attributes == NULL) {
		return errSecAllocate;
	}
	CFDictionarySetValue(attributes, kSecValueData, value);
	OSStatus status = SecItemAdd(attributes, NULL);
	CFRelease(attributes);
	return status;
}

static OSStatus jira_update(CFStringRef service, CFStringRef account, CFDataRef value) {
	CFMutableDictionaryRef query = jira_query(service, account);
	if (query == NULL) {
		return errSecAllocate;
	}
	const void *keys[] = { kSecValueData };
	const void *values[] = { value };
	CFDictionaryRef attributes = CFDictionaryCreate(
		kCFAllocatorDefault,
		keys,
		values,
		1,
		&kCFTypeDictionaryKeyCallBacks,
		&kCFTypeDictionaryValueCallBacks
	);
	if (attributes == NULL) {
		CFRelease(query);
		return errSecAllocate;
	}
	OSStatus status = SecItemUpdate(query, attributes);
	CFRelease(attributes);
	CFRelease(query);
	return status;
}

static OSStatus jira_delete(CFStringRef service, CFStringRef account) {
	CFMutableDictionaryRef query = jira_query(service, account);
	if (query == NULL) {
		return errSecAllocate;
	}
	OSStatus status = SecItemDelete(query);
	CFRelease(query);
	return status;
}
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"

	"github.com/abigotado/jira-cli/internal/profile"
)

// KeychainStore stores Jira tokens in the macOS login Keychain.
type KeychainStore struct{}

// Load retrieves the token for the exact profile account without allowing UI.
func (KeychainStore) Load(ctx context.Context, profileName string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if err := profile.ValidateName(profileName); err != nil {
		return Credential{}, err
	}
	service, releaseService, err := makeCFString(KeychainService)
	if err != nil {
		return Credential{}, err
	}
	defer releaseService()
	account, releaseAccount, err := makeCFString(profileName)
	if err != nil {
		return Credential{}, err
	}
	defer releaseAccount()

	var result C.CFTypeRef
	status := C.jira_load(service, account, &result)
	if status != C.errSecSuccess {
		return Credential{}, translateStatus("load", status)
	}
	defer C.CFRelease(result)
	data := C.CFDataRef(result)
	length := C.CFDataGetLength(data)
	if length <= 0 || length > C.CFIndex(MaxTokenBytes) {
		return Credential{}, fmt.Errorf("stored credential is invalid: %w", ErrInvalidToken)
	}
	bytes := C.CFDataGetBytePtr(data)
	credential := Credential{Token: C.GoStringN((*C.char)(unsafe.Pointer(bytes)), C.int(length))}
	if err := credential.Validate(); err != nil {
		return Credential{}, fmt.Errorf("stored credential is invalid: %w", err)
	}
	return credential, nil
}

// Save creates or updates the token for the exact profile account.
func (KeychainStore) Save(ctx context.Context, profileName string, credential Credential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := profile.ValidateName(profileName); err != nil {
		return err
	}
	if err := credential.Validate(); err != nil {
		return err
	}
	service, releaseService, err := makeCFString(KeychainService)
	if err != nil {
		return err
	}
	defer releaseService()
	account, releaseAccount, err := makeCFString(profileName)
	if err != nil {
		return err
	}
	defer releaseAccount()
	value := C.CFDataCreate(C.kCFAllocatorDefault, (*C.UInt8)(unsafe.Pointer(unsafe.StringData(credential.Token))), C.CFIndex(len(credential.Token)))
	if value == 0 {
		return &StatusError{Operation: "save", Status: int64(C.errSecAllocate)}
	}
	defer C.CFRelease(C.CFTypeRef(value))
	status := C.jira_add(service, account, value)
	if status == C.errSecDuplicateItem {
		status = C.jira_update(service, account, value)
	}
	if status != C.errSecSuccess {
		return translateStatus("save", status)
	}
	return nil
}

// Delete removes the token for the exact profile account without allowing UI.
func (KeychainStore) Delete(ctx context.Context, profileName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := profile.ValidateName(profileName); err != nil {
		return err
	}
	service, releaseService, err := makeCFString(KeychainService)
	if err != nil {
		return err
	}
	defer releaseService()
	account, releaseAccount, err := makeCFString(profileName)
	if err != nil {
		return err
	}
	defer releaseAccount()
	status := C.jira_delete(service, account)
	if status == C.errSecItemNotFound {
		return nil
	}
	if status != C.errSecSuccess {
		return translateStatus("delete", status)
	}
	return nil
}

func makeCFString(value string) (C.CFStringRef, func(), error) {
	bytes := unsafe.StringData(value)
	result := C.CFStringCreateWithBytes(
		C.kCFAllocatorDefault,
		(*C.UInt8)(unsafe.Pointer(bytes)),
		C.CFIndex(len(value)),
		C.kCFStringEncodingUTF8,
		C.false,
	)
	if result == 0 {
		return 0, func() {}, &StatusError{Operation: "allocate", Status: int64(C.errSecAllocate)}
	}
	return result, func() { C.CFRelease(C.CFTypeRef(result)) }, nil
}

func translateStatus(operation string, status C.OSStatus) error {
	switch status {
	case C.errSecItemNotFound:
		return ErrNotFound
	case C.errSecInteractionNotAllowed:
		return ErrInteractionNotAllowed
	default:
		return &StatusError{Operation: operation, Status: int64(status)}
	}
}
