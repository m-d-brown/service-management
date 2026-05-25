"""
test_inventory.py - Unit Test Suite for Reboot Orchestrator Inventory Parser
"""

import pytest
from reboot_orchestrator.inventory import load_inventory


def test_load_inventory(tmp_path: pytest.TempPathFactory) -> None:
    """
    Verifies nested group YAML structures are successfully loaded and flattened.
    """
    yaml_content = """
all:
  children:
    infrastructure:
      hosts:
        host-a:
          ip_addr: 10.0.0.1
    servers:
      children:
        application:
          hosts:
            host-b:
              ansible_user: admin
"""
    inv_file = tmp_path / "inventory.yml"  # type: ignore[operator]
    inv_file.write_text(yaml_content, encoding="utf-8")  # type: ignore[attr-defined]

    inventory = load_inventory(str(inv_file))

    assert "host-a" in inventory
    assert inventory["host-a"]["ip_addr"] == "10.0.0.1"
    assert "host-b" in inventory
    assert inventory["host-b"]["ansible_user"] == "admin"


def test_load_inventory_invalid_dependency(
    tmp_path: pytest.TempPathFactory,
) -> None:
    """
    Verifies load fails if a dependency references a non-existent host.
    """
    yaml_content = """
all:
  hosts:
    host-a:
      ip_addr: 10.0.0.1
      depends_on:
        - non-existent-host
"""
    inv_file = tmp_path / "inventory.yml"  # type: ignore[operator]
    inv_file.write_text(yaml_content, encoding="utf-8")  # type: ignore[attr-defined]
    with pytest.raises(
        ValueError, match="specifies a dependency on.*but.*does not exist"
    ):
        load_inventory(str(inv_file))
