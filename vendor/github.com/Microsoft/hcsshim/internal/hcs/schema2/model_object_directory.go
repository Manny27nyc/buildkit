/*
 * HCS API
 *
 * No description provided (generated by Swagger Codegen https://github.com/swagger-api/swagger-codegen)
 *
 * API version: 2.4
 * Generated by: Swagger Codegen (https://github.com/swagger-api/swagger-codegen.git)
 */

package hcsschema

type ObjectDirectory struct {
	Name    string            `json:"name,omitempty"`
	Clonesd string            `json:"clonesd,omitempty"`
	Shadow  string            `json:"shadow,omitempty"`
	Symlink []ObjectSymlink   `json:"symlink,omitempty"`
	Objdir  []ObjectDirectory `json:"objdir,omitempty"`
}
