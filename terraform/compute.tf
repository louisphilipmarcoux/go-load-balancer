resource "google_compute_instance" "lb_vm" {
  name         = "go-lb-vm"
  machine_type = "e2-micro" # The Always-Free machine type
  zone         = var.zone

  # This VM must be in a free-tier eligible region
  # e.g., us-west1, us-central1, us-east1

  tags = ["ssh-http-https"] # Applies our firewall rules

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12"
      size  = 30 # Part of the 30GB free tier
    }
  }

  network_interface {
    network = "default"
    access_config {
      // This assigns a public, non-static IP
    }
  }

  metadata = {
    # Add your SSH key
    ssh-keys = "louis:${file(var.ssh_public_key_path)}"

    # Run the startup script
    startup-script = file("${path.module}/startup-script.sh")
  }

  # The 'e2-micro' has 2 vCPUs that can burst, but are shared.
  # We must explicitly allow this.
  scheduling {
    provisioning_model = "STANDARD"
  }
}

output "vm_public_ip" {
  description = "The public IP address of the load balancer VM"
  value       = google_compute_instance.lb_vm.network_interface[0].access_config[0].nat_ip
}
