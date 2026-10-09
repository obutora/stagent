// Package pscwd holds the PowerShell snippet that keeps a shell's process
// working directory at its location.
//
// Windows PowerShell's Set-Location (sl, cd, cd..) moves the shell's own
// location but not the process's working directory, which is what the
// GitHub screen reads from the PEB (github.LiveDirs). Hook wraps the prompt
// function so that each prompt sets [Environment]::CurrentDirectory to the
// last file system location. The prompt it wraps (the default one,
// starship's, oh-my-posh's, …) is called first, so it still sees the
// command's $? and $LASTEXITCODE. Running it twice only wraps twice.
package pscwd

// Hook is one line of PowerShell; it wraps the prompt defined when it runs.
const Hook = `& { $prompt = $function:prompt; ` +
	`$function:global:prompt = { $out = & $prompt; ` +
	`try { [Environment]::CurrentDirectory = (Get-Location -PSProvider FileSystem).ProviderPath } catch {}; ` +
	`$out }.GetNewClosure() }`
