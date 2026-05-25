"""
inventory.py - Ansible Inventory Parser and Validator

This module handles loading, flattening, and validating Ansible inventory files
(YAML format) to retrieve host configurations and dependency declarations.
"""

from typing import Any
import os
import yaml


def load_inventory(inventory_path: str) -> dict[str, dict[str, Any]]:
    """
    Parses the inventory YAML file and recursively flattens the nested group hierarchy,
    extracting all host definitions into a single dictionary.

    Args:
        inventory_path: The file path to the Ansible inventory.yml file.

    Returns:
        dict[str, dict[str, Any]]: A mapping of hostname -> host properties
                                   (e.g., {'host-a': {'ip_addr': '10.0.0.1'}, ...}).

    Raises:
        FileNotFoundError: If the inventory file does not exist.
        yaml.YAMLError: If the inventory file contains invalid YAML syntax.
    """
    if not os.path.exists(inventory_path):
        raise FileNotFoundError(f"Cannot find inventory file at '{inventory_path}'.")

    with open(inventory_path, "r", encoding="utf-8") as f:
        inv = yaml.safe_load(f)

    if not inv:
        return {}

    hosts: dict[str, dict[str, Any]] = {}

    def extract(node: Any) -> None:
        if isinstance(node, dict):
            # Extract hosts defined at this level
            if "hosts" in node and isinstance(node["hosts"], dict):
                for h, props in node["hosts"].items():
                    hosts.setdefault(h, {}).update(props or {})
            # Recurse into nested children groups
            if "children" in node and isinstance(node["children"], dict):
                for child_node in node["children"].values():
                    extract(child_node)

    # Start recursion from the 'all' root group, or fallback to the whole inventory
    extract(inv.get("all", inv))

    # Validate that dependencies reference existing hosts
    for host, props in hosts.items():
        depends_on = props.get("depends_on")
        if depends_on is not None:
            if not isinstance(depends_on, list):
                raise ValueError(
                    f"Host '{host}' specifies an invalid 'depends_on' field (must be a list)."
                )
            for dep in depends_on:
                if dep not in hosts:
                    raise ValueError(
                        f"Host '{host}' specifies a dependency on '{dep}', "
                        f"but '{dep}' does not exist in the inventory."
                    )

    return hosts
