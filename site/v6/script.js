// Plumb v6 - Agentic Infrastructure Layer
// Dynamic UI Enhancements

document.addEventListener('DOMContentLoaded', () => {
    // Glitch effect randomization
    const glitchText = document.querySelector('.glitch-text');
    if (glitchText) {
        setInterval(() => {
            const skew = Math.random() * 20 - 10;
            const top = Math.random() * 100;
            const height = Math.random() * 50;
            
            glitchText.style.setProperty('--skew', `${skew}deg`);
            glitchText.style.setProperty('--top', `${top}%`);
            glitchText.style.setProperty('--height', `${height}%`);
        }, 150);
    }

    // Intersection Observer for scroll reveals
    const observerOptions = {
        threshold: 0.1
    };

    const observer = new IntersectionObserver((entries) => {
        entries.forEach(entry => {
            if (entry.isIntersecting) {
                entry.target.classList.add('in-view');
            }
        });
    }, observerOptions);

    document.querySelectorAll('section').forEach(section => {
        observer.observe(section);
    });

    // Handle smooth scrolling for anchors
    document.querySelectorAll('a[href^="#"]').forEach(anchor => {
        anchor.addEventListener('click', function (e) {
            e.preventDefault();
            const targetId = this.getAttribute('href').substring(1);
            const targetElement = document.getElementById(targetId);
            
            if (targetElement) {
                window.scrollTo({
                    top: targetElement.offsetTop - 80,
                    behavior: 'smooth'
                });
            }
        });
    });

    // Console Greeting
    console.log("%c PLUMB %c Initialized ", "color: #00E5FF; background: #01050a; font-weight: bold; padding: 2px 4px;", "color: #01050a; background: #00E5FF; font-weight: bold; padding: 2px 4px;");
});
