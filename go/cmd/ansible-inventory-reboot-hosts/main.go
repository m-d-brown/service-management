// Command ansible-inventory-reboot-hosts converts an Ansible YAML inventory
// into the host specs reboot-orchestrator reads.
//
// Usage:
//
//	ansible-inventory-reboot-hosts [--inventory FILE]
//
// It prints one spec per line on stdout, so a topology maintained in an
// inventory reaches the orchestrator through a pipe:
//
//	ansible-inventory-reboot-hosts -i inventory.yml | reboot-orchestrator vm-a vm-b
//
// Keeping the translation in its own command is what lets the orchestrator stay
// free of any inventory format: it reads host specs and nothing else, whether
// they come from here, from a file, or straight off the command line.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"

	"service-management/pkg/ansibleinv"
	"service-management/pkg/reboot"
)

func main() {
	log.SetFlags(0)

	inventory := flag.String("inventory", "inventory.yml", "path to the Ansible inventory file")
	flag.StringVar(inventory, "i", "inventory.yml", "path to the Ansible inventory file (shorthand)")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(),
			"Usage: %s [--inventory FILE]\n\n"+
				"Convert an Ansible YAML inventory into reboot-orchestrator host specs,\n"+
				"one per line on stdout. Reads ip_addr/ansible_host, ansible_user,\n"+
				"ansible_ssh_common_args, depends_on, runs_on, not_with and ready.\n\n"+
				"Pipe the result into reboot-orchestrator:\n"+
				"  %s -i inventory.yml | reboot-orchestrator vm-a vm-b\n\nFlags:\n",
			os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() > 0 {
		log.Fatalf("unexpected argument %q: this command takes no positional arguments", flag.Arg(0))
	}

	hosts, err := ansibleinv.Load(*inventory)
	if err != nil {
		log.Fatal(err)
	}

	out := bufio.NewWriter(os.Stdout)
	for _, host := range hosts {
		if _, err := fmt.Fprintln(out, reboot.FormatSpec(host)); err != nil {
			log.Fatalf("writing host specs: %v", err)
		}
	}
	if err := out.Flush(); err != nil {
		log.Fatalf("writing host specs: %v", err)
	}
}
